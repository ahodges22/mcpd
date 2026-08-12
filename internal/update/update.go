package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	githubOwner   = "ahodges22"
	githubRepo    = "mcpd"
	binaryName    = "mcpd"
	checksumName  = "checksums.txt"
	signatureName = "checksums.txt.cosign"
	userAgent     = "mcpd-updater/1"
	maxDownload   = 200 * 1024 * 1024
)

type Updater struct {
	BinaryPath     string
	CurrentVersion string
	HTTPClient     *http.Client
	VerifyBundle   func(context.Context, []byte, []byte, string) error
}

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CheckResult struct {
	Current  string
	Latest   string
	Outdated bool
}

type Options struct {
	Version string
}

func ArchiveName() string {
	return fmt.Sprintf("mcpd_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

func (updater *Updater) Check(ctx context.Context) (*CheckResult, error) {
	release, err := updater.release(ctx, "")
	if err != nil {
		return nil, err
	}
	return &CheckResult{
		Current:  updater.CurrentVersion,
		Latest:   release.TagName,
		Outdated: isOlder(updater.CurrentVersion, release.TagName),
	}, nil
}

func (updater *Updater) Update(ctx context.Context, opts Options) (string, error) {
	release, err := updater.release(ctx, opts.Version)
	if err != nil {
		return "", err
	}
	archiveName := ArchiveName()
	archive, err := updater.downloadAsset(ctx, release, archiveName)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", archiveName, err)
	}
	checksums, err := updater.downloadAsset(ctx, release, checksumName)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", checksumName, err)
	}
	bundle, err := updater.downloadAsset(ctx, release, signatureName)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", signatureName, err)
	}
	verifyBundle := updater.VerifyBundle
	if verifyBundle == nil {
		verifyBundle = verifyCosignBundle
	}
	if err := verifyBundle(ctx, bundle, checksums, release.TagName); err != nil {
		return "", fmt.Errorf("verify release signature: %w", err)
	}
	want, err := findChecksum(checksums, archiveName)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if !bytes.Equal(want, got[:]) {
		return "", fmt.Errorf("verify %s checksum: expected %x, got %x", archiveName, want, got)
	}
	binary, err := extractBinary(archive)
	if err != nil {
		return "", err
	}
	if err := swapInPlace(updater.BinaryPath, binary); err != nil {
		return "", err
	}
	return release.TagName, nil
}

func (updater *Updater) release(ctx context.Context, version string) (*Release, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	if version != "" {
		version = canonicalVersion(version)
		if !semver.IsValid(version) {
			return nil, fmt.Errorf("invalid release version %q", version)
		}
		endpoint = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", githubOwner, githubRepo, url.PathEscape(version))
	}
	var release Release
	if err := updater.getJSON(ctx, endpoint, &release); err != nil {
		return nil, err
	}
	if release.TagName == "" {
		return nil, errors.New("GitHub release has no tag")
	}
	if !semver.IsValid(release.TagName) {
		return nil, fmt.Errorf("GitHub release has invalid tag %q", release.TagName)
	}
	return &release, nil
}

func (updater *Updater) getJSON(ctx context.Context, endpoint string, value any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", userAgent)
	response, err := updater.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", endpoint, response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, maxDownload)).Decode(value)
}

func (updater *Updater) downloadAsset(ctx context.Context, release *Release, name string) ([]byte, error) {
	var asset *Asset
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			asset = &release.Assets[i]
			break
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("release asset not found: %s", name)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/assets/%d", githubOwner, githubRepo, asset.ID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", userAgent)
	response, err := updater.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", endpoint, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("release asset %s exceeds %d bytes", name, maxDownload)
	}
	return data, nil
}

func (updater *Updater) client() *http.Client {
	if updater.HTTPClient != nil {
		return updater.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func findChecksum(checksums []byte, name string) ([]byte, error) {
	pattern := regexp.MustCompile(`(?m)^([0-9a-fA-F]{64})\s+\*?` + regexp.QuoteMeta(name) + `\s*$`)
	match := pattern.FindSubmatch(checksums)
	if match == nil {
		return nil, fmt.Errorf("checksum entry for %q missing", name)
	}
	checksum, err := hex.DecodeString(string(match[1]))
	if err != nil {
		return nil, fmt.Errorf("decode checksum: %w", err)
	}
	return checksum, nil
}

func extractBinary(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if path.Base(header.Name) != binaryName || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			continue
		}
		if header.Size <= 0 || header.Size > maxDownload {
			return nil, fmt.Errorf("archive contains an invalid %s size: %d", binaryName, header.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tarReader, maxDownload+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from archive: %w", binaryName, err)
		}
		if len(data) == 0 || len(data) > maxDownload {
			return nil, fmt.Errorf("archive contains an invalid %s", binaryName)
		}
		return data, nil
	}
	return nil, fmt.Errorf("archive missing expected binary: %s", binaryName)
}

func swapInPlace(target string, data []byte) error {
	if len(data) == 0 {
		return errors.New("refusing to install an empty executable")
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular executable: %s", target)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".update-*")
	if err != nil {
		return fmt.Errorf("create update beside %s: %w", target, err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set update permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write update: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync update: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close update: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}
	removeTemporary = false
	return nil
}

func isOlder(current, latest string) bool {
	current = canonicalVersion(current)
	latest = canonicalVersion(latest)
	if !semver.IsValid(current) || !semver.IsValid(latest) {
		return current != latest
	}
	return semver.Compare(current, latest) < 0
}

func canonicalVersion(value string) string {
	if value != "" && !strings.HasPrefix(value, "v") {
		return "v" + value
	}
	return value
}
