//go:build linux

package solarbeam

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

type linuxUpdateRoundTrip func(*http.Request) (*http.Response, error)

func (fn linuxUpdateRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestLinuxEngineUpdateCompatibilityBeforeAnyInstalledFileChanges(t *testing.T) {
	for _, scenario := range []struct {
		name                       string
		signatureValid, compatible bool
	}{
		{"invalid-signature", false, true},
		{"unsupported-glibc-or-library", true, false},
		{"compatible", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			src := writeTarZstd(t, []tar.Header{
				{Name: "bundle/engine/sunshine", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
				{Name: "bundle/engine/solarbeam-bridge", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
			})
			artifact, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := verify.Ed25519PublicKeyFingerprint(public)
			if err != nil {
				t.Fatal(err)
			}
			signature := ed25519.Sign(private, artifact)
			if !scenario.signatureValid {
				signature[0] ^= 1
			}
			digest := sha256.Sum256(artifact)
			install := filepath.Join(t.TempDir(), "engine")
			if err := os.MkdirAll(install, 0o700); err != nil {
				t.Fatal(err)
			}
			current := filepath.Join(install, "sunshine")
			if err := os.WriteFile(current, []byte("working engine"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(install+".previous", 0o700); err != nil {
				t.Fatal(err)
			}
			previousMarker := filepath.Join(install+".previous", "retained")
			if err := os.WriteFile(previousMarker, []byte("previous engine"), 0o600); err != nil {
				t.Fatal(err)
			}
			oldClient, oldProbe := linuxEngineUpdateHTTPClient, linuxEngineCompatibilityFn
			t.Cleanup(func() { linuxEngineUpdateHTTPClient, linuxEngineCompatibilityFn = oldClient, oldProbe })
			linuxEngineUpdateHTTPClient = &http.Client{Transport: linuxUpdateRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(artifact)), Header: make(http.Header)}, nil
			})}
			probes := 0
			linuxEngineCompatibilityFn = func(_ context.Context, stage string) error {
				probes++
				if stage == install {
					t.Fatal("probe ran after activation")
				}
				if raw, err := os.ReadFile(current); err != nil || string(raw) != "working engine" {
					t.Fatal("working engine changed before compatibility probe")
				}
				if !scenario.compatible {
					return errors.New("GLIBC_2.44 not found")
				}
				return nil
			}
			err = applyEngineUpdate(context.Background(), Paths{Root: install, TrustedPublicKeys: map[string]ed25519.PublicKey{pin: public}}, proto.SolarbeamEngineUpdateArgs{URL: "https://release.example.test/engine", SHA256: hex.EncodeToString(digest[:]), SignerFingerprint: pin, Signature: base64.StdEncoding.EncodeToString(signature)})
			if !scenario.signatureValid {
				if err == nil || probes != 0 {
					t.Fatalf("invalid signature: error %v, probes %d", err, probes)
				}
			} else if !scenario.compatible {
				if err == nil || !strings.Contains(err.Error(), "compatibility") || probes != 1 {
					t.Fatalf("incompatible release: error %v, probes %d", err, probes)
				}
			} else {
				if err != nil || probes != 1 {
					t.Fatalf("compatible release: error %v, probes %d", err, probes)
				}
			}
			raw, err := os.ReadFile(current)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.signatureValid && scenario.compatible {
				if string(raw) != "binary" {
					t.Fatalf("update not activated: %s", raw)
				}
			} else {
				if string(raw) != "working engine" {
					t.Fatalf("rejected update replaced working engine: %s", raw)
				}
				if _, err := os.Stat(previousMarker); err != nil {
					t.Fatalf("rejected update removed previous backup: %v", err)
				}
			}
		})
	}
}
