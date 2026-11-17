package lightinstall

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportRedactionAndCanonicalContract(t *testing.T) {
	t.Setenv("USERNAME", "Alice")
	t.Setenv("USER", "alice")
	token := strings.Repeat("z", 43)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		for _, secret := range []string{"Alice", "alice@example.com", "rft_abc", "SECRET", "https://fleet.invalid/secret", token} {
			if strings.Contains(s, secret) {
				t.Errorf("report leaked %q in %s", secret, s)
			}
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing report credential")
		}
		if !strings.Contains(s, `"step":"connection"`) || !strings.Contains(s, `"errorCode":"CONNECTION_FAILED"`) {
			t.Errorf("bad report %s", s)
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	reportFailure(context.Background(), server.Client(), server.URL, Payload{FailureReportToken: token, PairingCodeID: "id"}, &Failure{Step: "connect", Cause: errors.New("Alice rft_abc Bearer SECRET https://fleet.invalid/secret " + token), LogLines: []string{`C:\Users\Alice alice@example.com`}})
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestReportRetriesOnlyTransientFailures(t *testing.T) {
	for _, status := range []int{400, 401, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) }))
			defer s.Close()
			reportFailure(context.Background(), s.Client(), s.URL, Payload{}, errors.New("test"))
			want := 1
			if status == 429 || status == 503 {
				want = 3
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}
func TestReportCancellationStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; cancel(); w.WriteHeader(503) }))
	defer s.Close()
	reportFailure(ctx, s.Client(), s.URL, Payload{}, errors.New("fail"))
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestRedactPrivateKeyAndSecrets(t *testing.T) {
	s := Redact("-----BEGIN RSA PRIVATE KEY-----\nTOP SECRET\n-----END RSA PRIVATE KEY-----")
	if strings.Contains(s, "SECRET") {
		t.Fatal(s)
	}
	for _, input := range []string{"rfc_ABCDEFGHJ", "password=secret123", "Bearer anything", "api_key: private"} {
		out := Redact(input)
		if out == input {
			t.Fatalf("unredacted: %s", out)
		}
	}
}
func TestPayloadReportingFieldsRemainStrict(t *testing.T) {
	base := `"enrollment_token":"rft_` + strings.Repeat("a", 43) + `"`
	for _, suffix := range []string{`,"resilient_install":true`, `,"failure_report_token":"` + strings.Repeat("z", 43) + `","pairing_code_id":"01234567-1234-1234-1234-123456789abc"`} {
		if _, err := ParsePayload([]byte("{" + base + suffix + "}")); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{`,"resilient_install":"true"`, `,"resilient_install":null`, `,"failure_report_token":null`, `,"expected_owner_actor_id":""`, `,"resilient_install":true,"resilient_install":false`, `,"failure_report_token":"bad"`, `,"pairing_code_id":"bad"`, `,"webhook":"https://evil"`} {
		if _, err := ParsePayload([]byte("{" + base + suffix + "}")); err == nil {
			t.Fatal("accepted " + suffix)
		}
	}
}

func TestReportEmptyLogsAreArrayAndPEMFragmentsNeverLeaveDevice(t *testing.T) {
	for _, logs := range [][]string{nil, {"-----BEGIN RSA PRIVATE KEY-----", "shortKeyMaterial", "-----END RSA PRIVATE KEY-----"}} {
		t.Run("wire", func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "shortKeyMaterial") || strings.Contains(string(b), `"lastLogLines":null`) {
					t.Fatalf("bad wire report: %s", b)
				}
				w.WriteHeader(202)
			}))
			defer s.Close()
			reportFailure(context.Background(), s.Client(), s.URL, Payload{}, &Failure{Step: "verify", Cause: errors.New("test"), LogLines: logs})
		})
	}
}

func TestRedactUsernamesInsideSecretsCannotSplitCredentials(t *testing.T) {
	t.Setenv("USER", "Alice")
	t.Setenv("USERNAME", "Alice")
	for _, secret := range []string{
		"rfp_aaaAliceBBBBCCCCDDDDEEEE",
		"QQQQQQQQAliceBBBBCCCCDDDDEEEEFFFFGGGG",
	} {
		out := redactBlock(secret)
		if out != "[redacted]" || strings.Contains(out, "QQQQQQQQ") || strings.Contains(out, "BBBBCCCC") {
			t.Fatalf("username replacement split a credential into surviving fragments: %q", out)
		}
	}
	if out := redactBlock("Setup for Alice stopped"); out != "Setup for [user] stopped" {
		t.Fatalf("plain username was not redacted: %q", out)
	}
}

func TestFailureReportUsesPublicAPIRoute(t *testing.T) {
	if reportEndpoint != "https://readyapp.player-ready.co.uk/api/solarbeam-devices/install-failure" {
		t.Fatalf("report endpoint misses public API router: %s", reportEndpoint)
	}
}
