package enroll

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testCert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func redeemServer(t *testing.T, profile string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"machine_id":"m1","venue_id":"v1","cert_pem":"` + "cert" + `","ca_pem":"ca","agent_listen_url":""`
		if profile != "" {
			body += `,"install_profile":"` + profile + `","device_tier":"byod"`
		}
		_, _ = w.Write([]byte(body + "}"))
	}))
}

// A redeem whose install_profile differs from ExpectInstallProfile fails
// closed: ErrInstallProfileMismatch and no material on disk.
func TestEnroll_ExpectInstallProfile(t *testing.T) {
	cases := []struct {
		name, server, expect string
		wantErr              bool
	}{
		{"match", "byod_solarbeam", "byod_solarbeam", false},
		{"mismatch", "managed", "byod_solarbeam", true},
		{"old server omits field", "", "byod_solarbeam", false},
		{"no expectation", "managed", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := redeemServer(t, tc.server)
			defer srv.Close()
			dir := t.TempDir()
			res, err := Enroll(context.Background(), Options{
				EnrollmentURL: srv.URL, BearerSecret: "rfc_ABCDEFGHJ", MaterialDir: filepath.Join(dir, "m"),
				HTTPClient: srv.Client(), ExpectInstallProfile: tc.expect,
			})
			if tc.wantErr {
				if !errors.Is(err, ErrInstallProfileMismatch) {
					t.Fatalf("err = %v, want ErrInstallProfileMismatch", err)
				}
				if _, statErr := os.Stat(filepath.Join(dir, "m", "client.key")); !errors.Is(statErr, os.ErrNotExist) {
					t.Error("material was written despite the mismatch")
				}
				return
			}
			if err != nil {
				t.Fatalf("Enroll: %v", err)
			}
			if res.InstallProfile != tc.server {
				t.Errorf("InstallProfile = %q, want %q", res.InstallProfile, tc.server)
			}
		})
	}
}
