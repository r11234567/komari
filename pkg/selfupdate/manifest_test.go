package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUpdateVerificationKeyUsesLocalConfiguration(t *testing.T) {
	for _, value := range []string{"", "invalid!", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		t.Setenv("KOMARI_UPDATE_SIGNING_PUBLIC_KEY", value)
		if _, err := updateVerificationKey(); err == nil {
			t.Fatal("accepted a missing or invalid release key")
		}
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOMARI_UPDATE_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	got, err := updateVerificationKey()
	if err != nil || !publicKey.Equal(got) {
		t.Fatalf("load configured key: %v", err)
	}
}

func TestVerifiedUpdateDownloadsAndExecutesOnlyAuthenticatedCandidate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd self-update is Linux-only")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const version, build = "2.0.5", "abc12d3"
	assetName := "komari-" + runtime.GOOS + "-" + runtime.GOARCH
	payload := []byte("#!/bin/sh\ntouch \"$(dirname \"$0\")/executed\"\nprintf '%s\\n' '{\"version\":\"2.0.5\",\"hash\":\"abc12d3\"}'\n")
	content, err := json.Marshal(Manifest{
		Schema: 1, Version: version, VersionHash: build,
		Assets: []ManifestAsset{{
			Name: assetName, OS: runtime.GOOS, Arch: runtime.GOARCH,
			Size: int64(len(payload)), SHA256: fmt.Sprintf("%x", sha256.Sum256(payload)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, content)
	tamperedPayload := append([]byte(nil), payload...)
	tamperedPayload[len(tamperedPayload)-1] ^= 1
	for _, scenario := range []struct {
		name          string
		key           ed25519.PublicKey
		signature     []byte
		manifest      []byte
		payload       []byte
		expectedBuild string
		wantError     string
		wantDownloads int32
	}{
		{"valid", publicKey, signature, content, payload, build, "", 1},
		{"missing signature", publicKey, nil, content, payload, build, "download update signature", 0},
		{"oversized signature", publicKey, make([]byte, ed25519.SignatureSize+1), content, payload, build, "too large", 0},
		{"untrusted signing key", otherKey, signature, content, payload, build, "signature verification failed", 0},
		{"altered manifest", publicKey, signature, append(append([]byte(nil), content...), '\n'), payload, build, "signature verification failed", 0},
		{"wrong signed selection", publicKey, signature, content, payload, "fffffff", "does not match the requested update", 0},
		{"altered binary", publicKey, signature, content, tamperedPayload, build, "checksum verification failed", 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var assetRequests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/" + signatureName:
					if scenario.signature == nil {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write(scenario.signature)
				case "/" + manifestName:
					_, _ = w.Write(scenario.manifest)
				case "/" + assetName:
					assetRequests.Add(1)
					_, _ = w.Write(scenario.payload)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			directory := t.TempDir()
			candidate := filepath.Join(directory, "candidate")
			err := stageVerifiedCandidate(t.Context(), server.Client(), server.URL, candidate, version, scenario.expectedBuild, scenario.key)
			_, markerErr := os.Stat(filepath.Join(directory, "executed"))
			if scenario.wantError == "" {
				if err != nil || markerErr != nil {
					t.Fatalf("verified candidate did not run: staging %v, execution %v", err, markerErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.wantError) || !os.IsNotExist(markerErr) {
				t.Fatalf("unexpected verification or execution: staging %v, execution %v", err, markerErr)
			}
			if got := assetRequests.Load(); got != scenario.wantDownloads {
				t.Fatalf("asset downloads = %d, want %d", got, scenario.wantDownloads)
			}
		})
	}
}
