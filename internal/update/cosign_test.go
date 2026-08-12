package update

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

func TestVerifyCosignBundleChecksSignatureAndRekorProof(t *testing.T) {
	checksums := []byte("checksums")
	bundle, trustedMaterial := testCosignBundle(t, checksums, releaseSubject("v1.2.3"))
	withTrustedMaterial(t, trustedMaterial)
	if err := verifyCosignBundle(context.Background(), bundle, checksums, "v1.2.3"); err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if err := verifyCosignBundle(context.Background(), bundle, []byte("tampered"), "v1.2.3"); err == nil {
		t.Fatal("tampered checksums accepted")
	}
}

func TestVerifyCosignBundleRequiresRekorProof(t *testing.T) {
	checksums := []byte("checksums")
	bundleJSON, trustedMaterial := testCosignBundle(t, checksums, releaseSubject("v1.2.3"))
	withTrustedMaterial(t, trustedMaterial)
	var bundle cosignBundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.RekorBundle = nil
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCosignBundle(context.Background(), bundleJSON, checksums, "v1.2.3"); err == nil {
		t.Fatal("bundle without a Rekor proof accepted")
	}
}

func TestVerifyCosignBundleChecksWorkflowIdentity(t *testing.T) {
	tests := []struct {
		subject string
		wantErr bool
	}{
		{subject: releaseSubject("v1.2.3")},
		{subject: "https://github.com/ahodges22/other/.github/workflows/release.yml@refs/tags/v1.2.3", wantErr: true},
		{subject: "https://github.com/ahodges22/mcpd/.github/workflows/ci.yml@refs/tags/v1.2.3", wantErr: true},
		{subject: "https://github.com/ahodges22/mcpd/.github/workflows/release.yml@refs/heads/main", wantErr: true},
		{subject: releaseSubject("v1.2.2"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.subject, func(t *testing.T) {
			checksums := []byte("checksums")
			bundle, trustedMaterial := testCosignBundle(t, checksums, test.subject)
			withTrustedMaterial(t, trustedMaterial)
			err := verifyCosignBundle(context.Background(), bundle, checksums, "v1.2.3")
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyCosignBundle() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestVerifyCosignBundlePassesCancellationToTrustedRoot(t *testing.T) {
	checksums := []byte("checksums")
	bundle, _ := testCosignBundle(t, checksums, releaseSubject("v1.2.3"))
	originalLoader := loadTrustedMaterial
	loadTrustedMaterial = func(ctx context.Context) (root.TrustedMaterial, error) {
		return nil, ctx.Err()
	}
	t.Cleanup(func() { loadTrustedMaterial = originalLoader })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyCosignBundle(ctx, bundle, checksums, "v1.2.3"); !errors.Is(err, context.Canceled) {
		t.Fatalf("verifyCosignBundle() error = %v, want context canceled", err)
	}
}

func TestTrustedRootFetcherBindsRequestsToContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})}
	fetcher := contextFetcher{ctx: ctx, client: client}
	if _, err := fetcher.DownloadFile("https://example.com/trusted_root.json", 1024, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("DownloadFile() error = %v, want context canceled", err)
	}
}

func testCosignBundle(t *testing.T, artifact []byte, subject string) ([]byte, root.TrustedMaterial) {
	t.Helper()
	virtualSigstore, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtualSigstore.SignAtTimeWithVersion(subject, cosignIssuer, artifact, time.Now().Add(5*time.Minute), "v0.1")
	if err != nil {
		t.Fatal(err)
	}
	verificationContent, err := entity.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	certificate := verificationContent.Certificate()
	if certificate == nil {
		t.Fatal("test entity missing certificate")
	}
	signatureContent, err := entity.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := entity.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("test entity has %d Rekor entries, want 1", len(entries))
	}
	body, ok := entries[0].Body().(string)
	if !ok {
		t.Fatal("test Rekor body is not a string")
	}
	payload := cosignRekorPayload{
		Body:           body,
		IntegratedTime: entries[0].IntegratedTime().Unix(),
		LogIndex:       entries[0].LogIndex(),
		LogID:          hex.EncodeToString([]byte(entries[0].LogKeyID())),
	}
	signedEntryTimestamp, err := virtualSigstore.RekorSignPayload(tlog.RekorPayload{
		Body:           payload.Body,
		IntegratedTime: payload.IntegratedTime,
		LogIndex:       payload.LogIndex,
		LogID:          payload.LogID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := json.Marshal(cosignBundle{
		Signature: base64.StdEncoding.EncodeToString(signatureContent.Signature()),
		Certificate: base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: certificate.Raw,
		})),
		RekorBundle: &cosignRekorBundle{
			Payload:              payload,
			SignedEntryTimestamp: base64.StdEncoding.EncodeToString(signedEntryTimestamp),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle, virtualSigstore
}

func withTrustedMaterial(t *testing.T, trustedMaterial root.TrustedMaterial) {
	t.Helper()
	originalLoader := loadTrustedMaterial
	originalVerifier := newSigstoreVerifier
	loadTrustedMaterial = func(context.Context) (root.TrustedMaterial, error) { return trustedMaterial, nil }
	newSigstoreVerifier = func(trustedMaterial root.TrustedMaterial) (*verify.Verifier, error) {
		return verify.NewVerifier(
			trustedMaterial,
			verify.WithTransparencyLog(1),
			verify.WithObserverTimestamps(1),
		)
	}
	t.Cleanup(func() {
		loadTrustedMaterial = originalLoader
		newSigstoreVerifier = originalVerifier
	})
}

func releaseSubject(version string) string {
	return "https://github.com/ahodges22/mcpd/.github/workflows/release.yml@refs/tags/" + version
}
