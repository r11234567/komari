package diagnosticrecords

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/r11234567/komari-proto/diagnostics"
)

const (
	MaxBundleSize = diagnostics.MaxEnvelopeSize + (16 << 10)
	bundleVersion = "komari.forensics.sealed.v1"
)

type manifest struct {
	Version    string    `json:"version"`
	Node       string    `json:"node"`
	Stream     string    `json:"stream"`
	Sequence   uint64    `json:"sequence"`
	Recipient  string    `json:"recipient_key_id"`
	Digest     string    `json:"ciphertext_sha256"`
	ReceivedAt time.Time `json:"received_at_utc"`
}

func bundleManifest(header diagnostics.Header, receipt diagnostics.Receipt) manifest {
	return manifest{
		Version: bundleVersion, Node: hex.EncodeToString(header.Node[:]), Stream: hex.EncodeToString(header.Stream[:]),
		Sequence: header.Sequence, Recipient: hex.EncodeToString(header.KeyID[:]), Digest: hex.EncodeToString(receipt.Digest[:]), ReceivedAt: receipt.ReceivedAt.UTC(),
	}
}

func writeBundle(dst io.Writer, header diagnostics.Header, receipt diagnostics.Receipt, envelope []byte) error {
	manifestBody, err := json.Marshal(bundleManifest(header, receipt))
	if err != nil {
		return err
	}
	receiptBody, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(dst)
	archive := tar.NewWriter(compressed)
	var writeErr error
	for _, item := range []struct {
		name string
		body []byte
	}{{"manifest.json", manifestBody}, {"receipt.json", receiptBody}, {"record.sealed", envelope}} {
		if writeErr = archive.WriteHeader(&tar.Header{Name: item.name, Size: int64(len(item.body)), Mode: 0o600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); writeErr != nil {
			break
		}
		if _, writeErr = archive.Write(item.body); writeErr != nil {
			break
		}
	}
	return errors.Join(writeErr, archive.Close(), compressed.Close())
}

// VerifyBundle checks the untouched envelope and metadata using the trusted
// receipt key. Verification never requires the panel's decryption key.
func VerifyBundle(src io.Reader, key ed25519.PublicKey) (diagnostics.Header, diagnostics.Receipt, []byte, error) {
	var supplied manifest
	var receipt diagnostics.Receipt
	compressed, err := gzip.NewReader(io.LimitReader(src, MaxBundleSize))
	if err != nil {
		return diagnostics.Header{}, receipt, nil, err
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	var envelope []byte
	for _, item := range []struct {
		name  string
		limit int64
		value any
	}{{"manifest.json", 4096, &supplied}, {"receipt.json", 4096, &receipt}, {"record.sealed", diagnostics.MaxEnvelopeSize, nil}} {
		header, err := archive.Next()
		if err != nil || header.Name != item.name || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > item.limit {
			return diagnostics.Header{}, receipt, nil, fmt.Errorf("invalid evidence archive entry %s", item.name)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			return diagnostics.Header{}, receipt, nil, err
		}
		if item.value == nil {
			envelope = body
		} else if err := json.Unmarshal(body, item.value); err != nil {
			return diagnostics.Header{}, receipt, nil, err
		}
	}
	if _, err := archive.Next(); !errors.Is(err, io.EOF) {
		return diagnostics.Header{}, receipt, nil, errors.New("archive contains unexpected entries")
	}
	if n, err := compressed.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		return diagnostics.Header{}, receipt, nil, errors.New("evidence archive has an invalid ending")
	}
	header, err := diagnostics.Inspect(envelope)
	if err != nil || receipt.Verify(key, envelope) != nil || supplied != bundleManifest(header, receipt) {
		return diagnostics.Header{}, receipt, nil, errors.New("evidence archive integrity verification failed")
	}
	return header, receipt, envelope, nil
}
