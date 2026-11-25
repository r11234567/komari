package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	releaseBaseURL = "https://github.com/r11234567/komari/releases/download"
	manifestName   = "komari-update.json"
	signatureName  = "komari-update.json.sig"
	maxManifest    = 2 << 20
	maxBinary      = 256 << 20
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	hashPattern    = regexp.MustCompile(`^[A-Za-z0-9]{7}$`)
)

type Manifest struct {
	Schema      int             `json:"schema"`
	Version     string          `json:"version"`
	VersionHash string          `json:"version_hash"`
	Assets      []ManifestAsset `json:"assets"`
}

type ManifestAsset struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func updateVerificationKey() (ed25519.PublicKey, error) {
	value := strings.TrimSpace(os.Getenv("KOMARI_UPDATE_SIGNING_PUBLIC_KEY"))
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("configure KOMARI_UPDATE_SIGNING_PUBLIC_KEY with the trusted Ed25519 release public key")
	}
	return ed25519.PublicKey(key), nil
}

func fetchUpdateMetadata(ctx context.Context, client *http.Client, endpoint string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update metadata returned HTTP %d", resp.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("update metadata is too large")
	}
	return content, nil
}

func fetchManifest(ctx context.Context, client *http.Client, manifestURL, signatureURL string, key ed25519.PublicKey) (*Manifest, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid update verification key")
	}
	signature, err := fetchUpdateMetadata(ctx, client, signatureURL, ed25519.SignatureSize)
	if err != nil {
		return nil, fmt.Errorf("download update signature: %w", err)
	}
	content, err := fetchUpdateMetadata(ctx, client, manifestURL, maxManifest)
	if err != nil {
		return nil, fmt.Errorf("download update manifest: %w", err)
	}
	if !ed25519.Verify(key, content, signature) {
		return nil, errors.New("update manifest signature verification failed")
	}
	var manifest Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("invalid update manifest: %w", err)
	}
	return &manifest, nil
}

func (manifest *Manifest) validate(version, versionHash string) (*ManifestAsset, error) {
	if manifest.Schema != 1 {
		return nil, fmt.Errorf("unsupported update manifest schema %d", manifest.Schema)
	}
	if manifest.Version != version || !strings.EqualFold(manifest.VersionHash, versionHash) {
		return nil, errors.New("release metadata does not match the requested update")
	}
	name := fmt.Sprintf("komari-%s-%s", runtime.GOOS, runtime.GOARCH)
	for i := range manifest.Assets {
		asset := &manifest.Assets[i]
		if asset.Name != name || asset.OS != runtime.GOOS || asset.Arch != runtime.GOARCH {
			continue
		}
		if asset.Size <= 0 || asset.Size > maxBinary {
			return nil, errors.New("invalid update asset size")
		}
		if len(asset.SHA256) != sha256.Size*2 {
			return nil, errors.New("invalid update asset checksum")
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return nil, errors.New("invalid update asset checksum")
		}
		return asset, nil
	}
	return nil, fmt.Errorf("release does not contain %s", name)
}

func downloadAsset(ctx context.Context, client *http.Client, url, destination string, asset ManifestAsset) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update asset returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > 0 && resp.ContentLength != asset.Size {
		return errors.New("update asset size does not match the manifest")
	}

	temporary := destination + ".download"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(resp.Body, asset.Size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != asset.Size {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return errors.New("update asset size does not match the manifest")
	}
	if !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), asset.SHA256) {
		_ = os.Remove(temporary)
		return errors.New("update asset checksum verification failed")
	}
	if err := os.Chmod(temporary, 0755); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, destination)
}

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}
