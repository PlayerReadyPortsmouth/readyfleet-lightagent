package staffmessage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func allow(*http.Request) error { return nil }

func do(t *testing.T, h http.Handler, method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPipeRefusesUnauthorisedClients(t *testing.T) {
	b := fastBroker()
	for name, h := range map[string]http.Handler{
		"nil authorize": Handler(b, nil),
		"refused":       Handler(b, func(*http.Request) error { return errors.New("wrong SID") }),
		"nil broker":    Handler(nil, allow),
	} {
		if rec := do(t, h, http.MethodGet, "/v1/notify/next", "", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: next = %d, want 403", name, rec.Code)
		}
	}
	if b.Present() {
		t.Fatal("a refused client counted as a present companion")
	}
}

func TestPipeRoutesAreExactlyTwo(t *testing.T) {
	h := Handler(fastBroker(), allow)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/notify/next"},
		{http.MethodGet, "/v1/notify/ack"},
		{http.MethodPost, "/v1/notify/send"},
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/notify/next?x=1"},
	} {
		if rec := do(t, h, tc.method, tc.path, "application/json", "{}"); rec.Code < 400 {
			t.Errorf("%s %s = %d, want refusal", tc.method, tc.path, rec.Code)
		}
	}
}

func TestPipeNextThenAck(t *testing.T) {
	b := fastBroker()
	b.AckTimeout = 2 * time.Second
	h := Handler(b, allow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recc := make(chan *httptest.ResponseRecorder, 1)
	go func() { recc <- do(t, h, http.MethodGet, "/v1/notify/next", "", "") }()
	waitPresent(t, b)
	type res struct {
		ack Ack
		err error
	}
	done := make(chan res, 1)
	go func() { a, err := b.Deliver(ctx, sample); done <- res{a, err} }()

	rec := <-recc
	if rec.Code != http.StatusOK {
		t.Fatalf("next = %d %s", rec.Code, rec.Body)
	}
	m, err := DecodeNext(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, f := Revalidate(m); f != nil {
		t.Fatalf("service sent a message the companion rejects: %v", f)
	}
	if got := do(t, h, http.MethodPost, "/v1/notify/ack", "text/plain", `{"id":"`+m.ID+`","displayed":true,"via":"banner"}`); got.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON ack = %d", got.Code)
	}
	for _, bad := range []string{
		`{"id":"` + m.ID + `","displayed":true,"via":"banner","extra":1}`,
		`{"id":"` + m.ID + `","displayed":true,"via":"tray"}`,
		`{"id":"` + m.ID + `","displayed":true,"via":"banner"} {}`,
		strings.Repeat(" ", MaxAckBytes+1),
	} {
		if got := do(t, h, http.MethodPost, "/v1/notify/ack", "application/json", bad); got.Code != http.StatusBadRequest {
			t.Fatalf("bad ack %q = %d, want 400", bad[:min(len(bad), 40)], got.Code)
		}
	}
	if got := do(t, h, http.MethodPost, "/v1/notify/ack", "application/json", `{"id":"`+m.ID+`","displayed":true,"via":"banner+tray"}`); got.Code != http.StatusNoContent {
		t.Fatalf("ack = %d %s", got.Code, got.Body)
	}
	r := <-done
	if r.err != nil || !r.ack.Displayed {
		t.Fatalf("deliver = %+v %v", r.ack, r.err)
	}
	if got := do(t, h, http.MethodPost, "/v1/notify/ack", "application/json", `{"id":"`+m.ID+`","displayed":true,"via":"banner"}`); got.Code != http.StatusNotFound {
		t.Fatalf("repeat ack = %d, want 404", got.Code)
	}
}

func TestPipeNextTimesOutWith204(t *testing.T) {
	b := fastBroker()
	b.PollWait = 20 * time.Millisecond
	if rec := do(t, Handler(b, allow), http.MethodGet, "/v1/notify/next", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("empty poll = %d, want 204", rec.Code)
	}
}

// A session change during the long-poll must not release the text, and the
// service must learn at once rather than waiting out the ack timeout.
func TestPipeReauthorisesBeforeReleasingMessage(t *testing.T) {
	b := fastBroker()
	b.AckTimeout = 5 * time.Second
	var calls atomic.Int32
	h := Handler(b, func(*http.Request) error {
		if calls.Add(1) > 1 {
			return errors.New("user switched")
		}
		return nil
	})
	recc := make(chan *httptest.ResponseRecorder, 1)
	go func() { recc <- do(t, h, http.MethodGet, "/v1/notify/next", "", "") }()
	waitPresent(t, b)
	start := time.Now()
	ack, err := b.Deliver(context.Background(), sample)
	rec := <-recc
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), sample.Body) {
		t.Fatalf("next = %d %q", rec.Code, rec.Body)
	}
	if err != nil || ack.Reason != ReasonSessionChanged {
		t.Fatalf("deliver = %+v %v", ack, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("session change waited for the ack timeout")
	}
}
