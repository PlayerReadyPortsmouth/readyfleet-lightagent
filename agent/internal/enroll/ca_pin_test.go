package enroll

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbeddedCAPEM_IsWellFormed proves the asset actually embedded in the
// binary (internal_ca.pem, fetched from the live C2's ca_material record —
// see ca_pin.go's doc comment) is a real, parseable certificate, and that it
// is in fact ReadyFleet's internal CA and not some other cert that happened
// to be in the file.
func TestEmbeddedCAPEM_IsWellFormed(t *testing.T) {
	block, rest := pem.Decode(embeddedCAPEM)
	if block == nil {
		t.Fatalf("embeddedCAPEM did not PEM-decode at all")
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("embeddedCAPEM PEM block type = %q, want CERTIFICATE", block.Type)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("embeddedCAPEM has unexpected trailing data after the certificate block: %q", rest)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("embeddedCAPEM did not parse as an X.509 certificate: %v", err)
	}

	if !cert.IsCA {
		t.Errorf("embedded cert is not marked as a CA (IsCA=false)")
	}
	if cert.Subject.CommonName != "ReadyFleet Internal CA" {
		t.Errorf("embedded cert Subject.CommonName = %q, want %q", cert.Subject.CommonName, "ReadyFleet Internal CA")
	}
	if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != "ReadyFleet" {
		t.Errorf("embedded cert Subject.Organization = %v, want [ReadyFleet]", cert.Subject.Organization)
	}
	if time.Now().After(cert.NotAfter) {
		t.Errorf("embedded CA cert already expired at %s", cert.NotAfter)
	}
}

// TestPinnedCAPool_ParsesFromEmbeddedPEM proves the exact mechanism Enroll
// relies on: AppendCertsFromPEM on the embedded bytes must succeed and
// produce a non-empty pool. mustPinnedCAPool already panics on failure at
// package init, so this is implicitly exercised just by the package
// loading at all; this test makes the assertion explicit and independently
// checkable rather than relying on an init-time panic to fail the suite.
func TestPinnedCAPool_ParsesFromEmbeddedPEM(t *testing.T) {
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(embeddedCAPEM); !ok {
		t.Fatalf("AppendCertsFromPEM(embeddedCAPEM) = false, want true")
	}
	if pool.Equal(x509.NewCertPool()) {
		t.Fatalf("pool is empty after a reported-successful AppendCertsFromPEM")
	}
	if pinnedCAPool == nil {
		t.Fatalf("package-level pinnedCAPool is nil")
	}
}

// TestBootstrapTLSConfig_PinsRootCAs proves Enroll's constructed tls.Config
// has RootCAs set to the pinned pool (not nil, which is what an empty
// tls.Config leaves it as — falling back to the OS system trust store) on
// the normal, non-insecure path, and that the dev-only Insecure bypass is
// untouched by this change.
func TestBootstrapTLSConfig_PinsRootCAs(t *testing.T) {
	t.Run("normal path pins RootCAs and does not skip verification", func(t *testing.T) {
		cfg := bootstrapTLSConfig(false)
		if cfg.RootCAs == nil {
			t.Fatalf("RootCAs is nil, want the pinned pool")
		}
		if !cfg.RootCAs.Equal(pinnedCAPool) {
			t.Errorf("RootCAs is not the pinned pool")
		}
		if cfg.InsecureSkipVerify {
			t.Errorf("InsecureSkipVerify = true on the non-insecure path")
		}
	})

	t.Run("insecure dev path still skips verification", func(t *testing.T) {
		cfg := bootstrapTLSConfig(true)
		if !cfg.InsecureSkipVerify {
			t.Errorf("InsecureSkipVerify = false with insecure=true, dev bypass regressed")
		}
	})
}

// testCA is a throwaway self-signed CA generated in-process for
// TestPinnedTrust_RejectsUnrelatedCA. Never the real embedded internal CA —
// this package deliberately never has that CA's private key.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T, cn string) testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert: cert, key: key, pool: pool}
}

// newTLSServerSignedBy starts an httptest TLS server whose leaf certificate
// is signed by ca.
func newTLSServerSignedBy(t *testing.T, ca testCA) *httptest.Server {
	t.Helper()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca.cert, &leafKey.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  leafKey,
		}},
	}
	srv.StartTLS()
	return srv
}

// TestPinnedTrust_RejectsUnrelatedCA is the stronger proof behind the pin:
// a client trusting only one CA's pool refuses a server certificate signed
// by a different, unrelated CA, and accepts one signed by the trusted CA.
// This exercises exactly the verification path bootstrapTLSConfig relies on
// (RootCAs-only trust, no system-pool fallback) — a substitute pool is
// passed directly here rather than swapping the real pinnedCAPool package
// var, so the test proves the general mechanism without ever needing the
// real internal CA's private key (which this package correctly never has).
func TestPinnedTrust_RejectsUnrelatedCA(t *testing.T) {
	trusted := newTestCA(t, "trusted-test-ca")
	unrelated := newTestCA(t, "unrelated-test-ca")

	srv := newTLSServerSignedBy(t, trusted)
	defer srv.Close()

	t.Run("client trusting the signing CA succeeds", func(t *testing.T) {
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trusted.pool}},
		}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("expected handshake to succeed against the trusted CA, got: %v", err)
		}
		resp.Body.Close()
	})

	t.Run("client trusting only an unrelated CA is refused", func(t *testing.T) {
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: unrelated.pool}},
		}
		_, err := client.Get(srv.URL)
		if err == nil {
			t.Fatalf("expected the handshake to fail against an unrelated CA pool, got nil error")
		}
		var uae x509.UnknownAuthorityError
		if !errors.As(err, &uae) {
			t.Errorf("expected an x509.UnknownAuthorityError somewhere in the chain, got (%T): %v", err, err)
		}
	})
}
