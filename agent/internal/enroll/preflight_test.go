package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreflightCode_SendsBareCodeAndParses(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/enroll/v1/preflight" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("preflight must not carry a bearer: it spends nothing")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"install_profile":"managed","device_tier":"home","venue_name":"Plymouth","assigned_user_display":"D. Example"}`))
	}))
	defer srv.Close()

	pf, err := PreflightCode(context.Background(), PreflightOptions{URL: srv.URL + "/enroll/v1/preflight", Code: "ABCDEFGHJ", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["code"] != "ABCDEFGHJ" || len(gotBody) != 1 {
		t.Errorf("body = %v", gotBody)
	}
	if pf.InstallProfile != ProfileManaged || pf.DeviceTier != TierHome || pf.VenueName != "Plymouth" || pf.AssignedUserDisplay != "D. Example" {
		t.Errorf("preflight = %+v", pf)
	}
}

func TestPreflightCode_Statuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusNotFound, `{"error":"not_found"}`, ErrCodeNotFound},
		{http.StatusTooManyRequests, ``, ErrPreflightUnavailable},
		{http.StatusInternalServerError, ``, ErrPreflightUnavailable},
		{http.StatusOK, `not json`, ErrPreflightUnavailable},
		{http.StatusOK, `{"venue_name":"x"}`, ErrPreflightUnavailable},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := PreflightCode(context.Background(), PreflightOptions{URL: srv.URL, Code: "ABCDEFGHJ", HTTPClient: srv.Client()})
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d body %q: err %v, want %v", tc.status, tc.body, err, tc.want)
		}
	}
}

func TestPreflightURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://fleet.player-ready.co.uk:8643/agent/v1": "https://fleet.player-ready.co.uk:8643/enroll/v1/preflight",
		"ws://localhost:8643/agent/v1":                 "https://localhost:8643/enroll/v1/preflight",
	} {
		if got := PreflightURL(in); got != want {
			t.Errorf("PreflightURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedeemURL("wss://h:1/agent/v1"); got != "https://h:1/enroll/v1/redeem" {
		t.Errorf("RedeemURL = %q", got)
	}
}
