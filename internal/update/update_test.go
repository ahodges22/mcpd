package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArchiveNameMatchesReleaseConfig(t *testing.T) {
	want := fmt.Sprintf("mcpd_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if got := ArchiveName(); got != want {
		t.Fatalf("ArchiveName() = %q, want %q", got, want)
	}

	config, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`name_template: "{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}"`,
		`name_template: checksums.txt`,
		`signature: ${artifact}.cosign`,
		`--bundle=${signature}`,
	} {
		if !bytes.Contains(config, []byte(fragment)) {
			t.Errorf("GoReleaser config does not contain %q", fragment)
		}
	}
}

func TestCheckUsesSemanticVersionOrdering(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/repos/ahodges22/mcpd/releases/latest" {
			t.Fatalf("release path = %q", request.URL.Path)
		}
		return testResponse(http.StatusOK, `{"tag_name":"v1.3.0","assets":[]}`), nil
	})}
	updater := &Updater{CurrentVersion: "v1.2.0", HTTPClient: client}

	result, err := updater.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != "v1.2.0" || result.Latest != "v1.3.0" || !result.Outdated {
		t.Fatalf("Check() = %+v", result)
	}
}

func TestDefaultClientUsesHTTP1(t *testing.T) {
	protocol := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		protocol <- request.Proto
		response.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	client := (&Updater{}).client()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client transport = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil || len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatalf("default client TLS protocols = %v, want [http/1.1]", transport.TLSClientConfig)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConfig := transport.TLSClientConfig.Clone()
	tlsConfig.RootCAs = roots
	transport.TLSClientConfig = tlsConfig
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := <-protocol; got != "HTTP/1.1" {
		t.Fatalf("request protocol = %q, want HTTP/1.1", got)
	}
}

func TestUpdateVerifiesReleaseAndReplacesBinary(t *testing.T) {
	archive := testArchive(t, "mcpd", []byte("new binary"))
	digest := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%x  %s\n", digest, ArchiveName())
	client := releaseClient(t, archive, checksums, []byte("signature"))
	verified := false
	target := filepath.Join(t.TempDir(), "mcpd")
	if err := os.WriteFile(target, []byte("old binary"), 0o751); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{
		BinaryPath: target,
		HTTPClient: client,
		VerifyBundle: func(_ context.Context, bundle, artifact []byte, tag string) error {
			verified = bytes.Equal(bundle, []byte("signature")) && bytes.Equal(artifact, []byte(checksums)) && tag == "v1.3.0"
			return nil
		},
	}

	tag, err := updater.Update(context.Background(), Options{Version: "1.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.3.0" || !verified {
		t.Fatalf("Update() = %q, verified = %v", tag, verified)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new binary" {
		t.Fatalf("updated binary = %q", content)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("updated mode = %04o", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".mcpd.update-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, error = %v", matches, err)
	}
}

func TestUpdateDoesNotReplaceBinaryWhenSignatureFails(t *testing.T) {
	archive := testArchive(t, "mcpd", []byte("new binary"))
	digest := sha256.Sum256(archive)
	client := releaseClient(t, archive, fmt.Sprintf("%x  %s\n", digest, ArchiveName()), []byte("bad signature"))
	target := filepath.Join(t.TempDir(), "mcpd")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{
		BinaryPath:   target,
		HTTPClient:   client,
		VerifyBundle: func(context.Context, []byte, []byte, string) error { return fmt.Errorf("untrusted signer") },
	}

	if _, err := updater.Update(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "verify release signature") {
		t.Fatalf("Update() error = %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old binary" {
		t.Fatalf("existing binary changed to %q", content)
	}
}

func TestExtractBinaryRejectsDirectoryEntry(t *testing.T) {
	archive := testArchiveWithType(t, "mcpd/", nil, tar.TypeDir)
	if _, err := extractBinary(archive); err == nil {
		t.Fatal("directory entry accepted as executable")
	}
}

func TestSwapInPlaceRejectsEmptyData(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mcpd")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapInPlace(target, nil); err == nil {
		t.Fatal("empty executable accepted")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old binary" {
		t.Fatalf("existing binary changed to %q", content)
	}
}

func releaseClient(t *testing.T, archive []byte, checksums string, signature []byte) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/repos/ahodges22/mcpd/releases/latest", "/repos/ahodges22/mcpd/releases/tags/v1.3.0":
			body := fmt.Sprintf(`{"tag_name":"v1.3.0","assets":[{"id":1,"name":%q},{"id":2,"name":"checksums.txt"},{"id":3,"name":"checksums.txt.cosign"}]}`, ArchiveName())
			return testResponse(http.StatusOK, body), nil
		case "/repos/ahodges22/mcpd/releases/assets/1":
			return testBytesResponse(http.StatusOK, archive), nil
		case "/repos/ahodges22/mcpd/releases/assets/2":
			return testResponse(http.StatusOK, checksums), nil
		case "/repos/ahodges22/mcpd/releases/assets/3":
			return testBytesResponse(http.StatusOK, signature), nil
		default:
			t.Fatalf("unexpected request path %q", request.URL.Path)
			return nil, nil
		}
	})}
}

func testArchive(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	return testArchiveWithType(t, name, data, tar.TypeReg)
}

func testArchiveWithType(t *testing.T, name string, data []byte, typeflag byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: typeflag}); err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 {
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testResponse(status int, body string) *http.Response {
	return testBytesResponse(status, []byte(body))
}

func testBytesResponse(status int, body []byte) *http.Response {
	return &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}
