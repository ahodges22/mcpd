package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

const (
	cosignIssuer        = "https://token.actions.githubusercontent.com"
	cosignSubjectPrefix = "https://github.com/ahodges22/mcpd/.github/workflows/release.yml@refs/tags/"
	legacyBundleType    = "application/vnd.dev.sigstore.bundle+json;version=0.1"
)

type cosignBundle struct {
	Signature   string             `json:"base64Signature"`
	Certificate string             `json:"cert"`
	RekorBundle *cosignRekorBundle `json:"rekorBundle"`
}

type cosignRekorBundle struct {
	Payload              cosignRekorPayload `json:"Payload"`
	SignedEntryTimestamp string             `json:"SignedEntryTimestamp"`
}

type cosignRekorPayload struct {
	Body           string `json:"body"`
	IntegratedTime int64  `json:"integratedTime"`
	LogIndex       int64  `json:"logIndex"`
	LogID          string `json:"logID"`
}

var loadTrustedMaterial = func(ctx context.Context) (root.TrustedMaterial, error) {
	options := tuf.DefaultOptions().WithContext(ctx).WithFetcher(contextFetcher{ctx: ctx, client: http1Client(5 * time.Minute)})
	return root.FetchTrustedRootWithOptions(options)
}

var newSigstoreVerifier = func(trustedMaterial root.TrustedMaterial) (*verify.Verifier, error) {
	return verify.NewVerifier(
		trustedMaterial,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
}

func verifyCosignBundle(ctx context.Context, bundleJSON, checksums []byte, tag string) error {
	signedEntity, err := loadCosignBundle(bundleJSON, checksums)
	if err != nil {
		return err
	}
	trustedMaterial, err := loadTrustedMaterial(ctx)
	if err != nil {
		return fmt.Errorf("load Sigstore trusted root: %w", err)
	}
	verifier, err := newSigstoreVerifier(trustedMaterial)
	if err != nil {
		return fmt.Errorf("create Sigstore verifier: %w", err)
	}
	expected, err := verify.NewShortCertificateIdentity(cosignIssuer, "", cosignSubjectPrefix+tag, "")
	if err != nil {
		return fmt.Errorf("create certificate identity: %w", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(checksums)),
		verify.WithCertificateIdentity(expected),
	)
	if _, err := verifier.Verify(signedEntity, policy); err != nil {
		return fmt.Errorf("verify Sigstore bundle: %w", err)
	}
	return nil
}

type contextFetcher struct {
	ctx    context.Context
	client *http.Client
}

func (fetcher contextFetcher) DownloadFile(url string, maxLength int64, _ time.Duration) ([]byte, error) {
	request, err := http.NewRequestWithContext(fetcher.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: response.StatusCode, URL: url}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLength+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: fmt.Sprintf("download failed for %s: exceeds %d bytes", url, maxLength)}
	}
	return data, nil
}

func loadCosignBundle(bundleJSON, checksums []byte) (*sigbundle.Bundle, error) {
	var bundle cosignBundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return nil, fmt.Errorf("decode cosign bundle: %w", err)
	}
	if bundle.RekorBundle == nil {
		return nil, errors.New("cosign bundle missing Rekor proof")
	}
	certificate, err := bundle.certificate()
	if err != nil {
		return nil, err
	}
	signature, err := decodeBundleValue("signature", bundle.Signature)
	if err != nil {
		return nil, err
	}
	body, err := decodeBundleValue("Rekor body", bundle.RekorBundle.Payload.Body)
	if err != nil {
		return nil, err
	}
	logID, err := hex.DecodeString(bundle.RekorBundle.Payload.LogID)
	if err != nil || len(logID) == 0 {
		return nil, errors.New("decode Rekor log ID")
	}
	signedEntryTimestamp, err := decodeBundleValue("Rekor signed entry timestamp", bundle.RekorBundle.SignedEntryTimestamp)
	if err != nil {
		return nil, err
	}
	if bundle.RekorBundle.Payload.IntegratedTime <= 0 {
		return nil, errors.New("cosign bundle missing Rekor integrated time")
	}
	if bundle.RekorBundle.Payload.LogIndex < 0 {
		return nil, errors.New("cosign bundle has invalid Rekor log index")
	}

	var entryHeader struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(body, &entryHeader); err != nil {
		return nil, fmt.Errorf("decode Rekor body: %w", err)
	}
	entry, err := tlog.NewEntry(
		body,
		bundle.RekorBundle.Payload.IntegratedTime,
		bundle.RekorBundle.Payload.LogIndex,
		logID,
		signedEntryTimestamp,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("decode Rekor entry: %w", err)
	}
	transparencyEntry := entry.TransparencyLogEntry()
	transparencyEntry.KindVersion = &protorekor.KindVersion{
		Kind:    entryHeader.Kind,
		Version: entryHeader.APIVersion,
	}
	transparencyEntry.InclusionPromise = &protorekor.InclusionPromise{
		SignedEntryTimestamp: signedEntryTimestamp,
	}
	digest := sha256.Sum256(checksums)
	return sigbundle.NewBundle(&protobundle.Bundle{
		MediaType: legacyBundleType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{{RawBytes: certificate.Raw}},
				},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{transparencyEntry},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digest[:],
				},
				Signature: signature,
			},
		},
	})
}

func decodeBundleValue(name, value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return nil, fmt.Errorf("decode %s", name)
	}
	return decoded, nil
}

func (bundle cosignBundle) certificate() (*x509.Certificate, error) {
	data, err := decodeBundleValue("certificate", bundle.Certificate)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("decode certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return certificate, nil
}
