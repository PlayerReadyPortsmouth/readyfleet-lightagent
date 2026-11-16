package venue

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrivacyStateLifecycle(t *testing.T) {
	var p PrivacyState
	now := time.Now()
	if p.Current(now).Active {
		t.Fatal("zero state must be inactive")
	}
	p.Begin("s1", "Aurora N.", true, now)
	if v := p.Current(now); !v.Active || v.Label != "Aurora N." || !v.Input || !p.Matches("s1") {
		t.Fatalf("view = %+v", v)
	}
	if p.End("other") {
		t.Fatal("a different session must not clear it")
	}
	if !p.End("s1") || p.Current(now).Active {
		t.Fatal("End did not clear")
	}
}

func TestPrivacyStateExpiresAtHardCap(t *testing.T) {
	var p PrivacyState
	now := time.Now()
	p.Begin("s1", "A B.", false, now)
	if p.Current(now.Add(PrivacyMaxAge + time.Second)).Active {
		t.Fatal("privacy outlived its hard cap")
	}
}

func TestPrivacyOutcomeRoute(t *testing.T) {
	var got []PrivacyOutcome
	store := &Store{OnPrivacyOutcome: func(o PrivacyOutcome) { got = append(got, o) }}
	h := Handler(store)
	post := func(body string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/privacy-outcome", strings.NewReader(body)))
		return rr.Code
	}
	if c := post(`{"sessionId":"s1","outcome":"partial","monitors":2,"covered":1}`); c != 204 || len(got) != 1 {
		t.Fatalf("valid outcome: code %d got %v", c, got)
	}
	for _, bad := range []string{`{"sessionId":"","outcome":"on"}`, `{"sessionId":"s","outcome":"hax"}`, `{"sessionId":"s","outcome":"on","monitors":1,"covered":2}`, `nope`} {
		if c := post(bad); c != 400 {
			t.Errorf("%s: code %d, want 400", bad, c)
		}
	}
	rr := httptest.NewRecorder()
	Handler(&Store{}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/privacy-outcome", strings.NewReader(`{}`)))
	if rr.Code != 405 {
		t.Fatalf("route without a sink must stay read-only, got %d", rr.Code)
	}
}

func TestPrivacyReconcile(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		current string
		at      time.Time
		want    bool
	}{
		{"session starting, inside grace", "", now.Add(time.Minute), true},
		{"same session live", "s1", now.Add(time.Hour), true},
		{"different session live", "s2", now.Add(time.Second), false},
		{"no session after grace", "", now.Add(StartGrace + time.Second), false},
	}
	for _, c := range cases {
		var p PrivacyState
		p.Begin("s1", "A B.", false, now)
		p.Reconcile(c.current, c.at)
		if got := p.Current(c.at).Active; got != c.want {
			t.Errorf("%s: active = %v, want %v", c.name, got, c.want)
		}
	}
}
