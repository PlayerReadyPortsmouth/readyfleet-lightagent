package modeswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	testMachine = "0b0a9cf8-4bba-42b5-b74c-03cfb46819bc"
	testVenue   = "1c1a9cf8-4bba-42b5-b74c-03cfb46819bc"
)

type fakeExecutor struct {
	mu      sync.Mutex
	status  *proto.OperatingModeEnforcementStatus
	calls   []proto.OperatingModeTransitionRequest
	sources []string
	release chan struct{}
}

func (f *fakeExecutor) EnforcementStatus() *proto.OperatingModeEnforcementStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		return nil
	}
	c := *f.status
	return &c
}

func (f *fakeExecutor) Transition(_ context.Context, req proto.OperatingModeTransitionRequest, source string) proto.OperatingModeTransitionReport {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.sources = append(f.sources, source)
	release := f.release
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	return proto.OperatingModeTransitionReport{RequestID: req.RequestID, To: req.Grant.TargetMode, Outcome: proto.OperatingModeOutcomeSwitched}
}

func (f *fakeExecutor) snapshot() ([]proto.OperatingModeTransitionRequest, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.OperatingModeTransitionRequest(nil), f.calls...), append([]string(nil), f.sources...)
}

func (c *fakeC2) requests() []proto.OperatingModeGrantRequestData {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]proto.OperatingModeGrantRequestData(nil), c.seen...)
}

func owned() *proto.OperatingModeEnforcementStatus {
	return &proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseOwned, Current: proto.OperatingModeHeld, VenueID: testVenue, ListRevision: 7, Journal: "idle"}
}

// fakeC2 answers each grant request through the service's own response
// handler, the way the transport does.
type fakeC2 struct {
	t       *testing.T
	mu      sync.Mutex
	svc     *Service
	seen    []proto.OperatingModeGrantRequestData
	answer  func(proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData
	sendErr error
	// reports are MsgOperatingModeTransitionReport payloads; reportFails makes
	// the first N report sends fail.
	reports     []proto.OperatingModeTransitionReport
	reportFails int
}

func (c *fakeC2) transitionReports() []proto.OperatingModeTransitionReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]proto.OperatingModeTransitionReport(nil), c.reports...)
}

func (c *fakeC2) send(_ context.Context, env proto.Envelope) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	if env.Type == proto.MsgOperatingModeTransitionReport {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.reportFails > 0 {
			c.reportFails--
			return errors.New("not connected")
		}
		var rep proto.OperatingModeTransitionReport
		if err := env.DecodeData(&rep); err != nil {
			c.t.Fatal(err)
		}
		c.reports = append(c.reports, rep)
		return nil
	}
	if env.Type != proto.MsgOperatingModeGrantRequest {
		c.t.Errorf("sent %s", env.Type)
	}
	var req proto.OperatingModeGrantRequestData
	if err := env.DecodeData(&req); err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.seen = append(c.seen, req)
	c.mu.Unlock()
	if resp := c.answer(req); resp != nil {
		out, _ := proto.NewEnvelope(proto.MsgOperatingModeGrantResponse, uuid.Nil, &env.ID, resp)
		go c.svc.HandleGrantResponse(context.Background(), out)
	}
	return nil
}

func grantFor(req proto.OperatingModeGrantRequestData, now time.Time) *proto.OperatingModeGrant {
	return &proto.OperatingModeGrant{GrantID: uuid.NewString(), MachineID: testMachine, VenueID: testVenue, TargetMode: req.TargetMode,
		ListRevision: req.ListRevision, Nonce: strings.Repeat("ab", 16), IssuedAt: now, ExpiresAt: now.Add(60 * time.Second),
		Source: proto.OperatingModeGrantSourceLocalPIN, Actor: "venue_pin:" + testVenue}
}

func newTestService(t *testing.T, exec *fakeExecutor, answer func(proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData, logs io.Writer) (*Service, *fakeC2) {
	t.Helper()
	c2 := &fakeC2{t: t, answer: answer}
	if logs == nil {
		logs = io.Discard
	}
	svc := New(Options{MachineID: testMachine, Send: c2.send, Executor: exec, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		Connection: func() (bool, time.Time) { return true, time.Now() }, AnswerTimeout: 500 * time.Millisecond,
		ReportRetry: []time.Duration{time.Millisecond, time.Millisecond}})
	c2.svc = svc
	return svc, c2
}

func pinBytes(s string) *[4]byte {
	var p [4]byte
	copy(p[:], s)
	return &p
}

func waitState(t *testing.T, svc *Service, state string) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := svc.Status(); st.Request.State == state {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state never became %s: %+v", state, svc.Status().Request)
	return Status{}
}

func TestRequestGrantedHandsToExecutorAsLocalPIN(t *testing.T) {
	var logs bytes.Buffer
	exec := &fakeExecutor{status: owned()}
	svc, c2 := newTestService(t, exec, func(req proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
		return &proto.OperatingModeGrantResponseData{RequestID: req.RequestID, Granted: true, Grant: grantFor(req, time.Now())}
	}, &logs)
	pin := pinBytes("4826")
	res := svc.Request(context.Background(), context.Background(), proto.OperatingModeVR, pin)
	if !res.Accepted || res.Code != "" {
		t.Fatalf("result = %+v", res)
	}
	if *pin != [4]byte{} {
		t.Fatal("PIN not wiped")
	}
	st := waitState(t, svc, StateDone)
	if st.Request.Report == nil || st.Request.Report.Outcome != proto.OperatingModeOutcomeSwitched {
		t.Fatalf("status = %+v", st.Request)
	}
	seen := c2.requests()
	if len(seen) != 1 || seen[0].PIN != "4826" || seen[0].ListRevision != 7 || seen[0].TargetMode != "vr" {
		t.Fatalf("C2 saw %+v", seen)
	}
	calls, sources := exec.snapshot()
	if len(calls) != 1 || sources[0] != proto.OperatingModeGrantSourceLocalPIN || calls[0].RequestID != seen[0].RequestID {
		t.Fatalf("executor calls = %+v %v", calls, sources)
	}
	raw, _ := json.Marshal(svc.Status())
	if strings.Contains(string(raw), "4826") || strings.Contains(string(raw), calls[0].Grant.GrantID) {
		t.Fatalf("status leaks PIN or grant: %s", raw)
	}
	if strings.Contains(logs.String(), "4826") {
		t.Fatal("PIN logged")
	}
}

func TestRequestRefusalsNeverReachExecutor(t *testing.T) {
	now := time.Now()
	remaining := 4
	until := now.Add(10 * time.Minute)
	cases := map[string]struct {
		status  *proto.OperatingModeEnforcementStatus
		offline bool
		sendErr error
		answer  func(proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData
		code    string
		sent    int
	}{
		"not enforced": {status: nil, code: CodeNotEnforced},
		"partial":      {status: &proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhasePartial, ListRevision: 7}, code: CodeNotEnforced},
		"offline":      {status: owned(), offline: true, code: CodeOffline},
		"send fails":   {status: owned(), sendErr: errors.New("closed"), code: CodeOffline},
		"no answer":    {status: owned(), code: CodeNoAnswer, sent: 1, answer: func(proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData { return nil }},
		"wrong pin": {status: owned(), code: proto.OperatingModeGrantCodeWrongPIN, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Code: "wrong_pin", Reason: "That PIN wasn't accepted.", AttemptsRemaining: &remaining}
		}},
		"locked": {status: owned(), code: proto.OperatingModeGrantCodeLockedOut, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Code: "locked_out", LockedUntil: &until}
		}},
		"grant other machine": {status: owned(), code: CodeGrantInvalid, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			g := grantFor(r, now)
			g.MachineID = uuid.NewString()
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Granted: true, Grant: g}
		}},
		"grant other mode": {status: owned(), code: CodeGrantInvalid, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			g := grantFor(r, now)
			g.TargetMode = proto.OperatingModeVR
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Granted: true, Grant: g}
		}},
		"grant expired": {status: owned(), code: CodeGrantInvalid, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Granted: true, Grant: grantFor(r, now.Add(-3*time.Minute))}
		}},
		"grant remote source": {status: owned(), code: CodeGrantInvalid, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			g := grantFor(r, now)
			g.Source, g.Actor = proto.OperatingModeGrantSourceRemoteStaff, "aurora"
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Granted: true, Grant: g}
		}},
		"granted without grant": {status: owned(), code: CodeGrantInvalid, sent: 1, answer: func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
			return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Granted: true}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExecutor{status: c.status}
			answer := c.answer
			if answer == nil {
				answer = func(proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData { return nil }
			}
			svc, c2 := newTestService(t, exec, answer, nil)
			c2.sendErr = c.sendErr
			if c.offline {
				svc.opts.Connection = func() (bool, time.Time) { return false, time.Time{} }
			}
			pin := pinBytes("1357")
			res := svc.Request(context.Background(), context.Background(), proto.OperatingModeAPClubs, pin)
			if res.Accepted || res.Code != c.code || *pin != [4]byte{} {
				t.Fatalf("result = %+v pin=%v", res, *pin)
			}
			if calls, _ := exec.snapshot(); len(c2.requests()) != c.sent || len(calls) != 0 {
				t.Fatalf("sent=%d executor=%d", len(c2.requests()), len(calls))
			}
			if st := svc.Status(); st.Request.State != StateRefused || st.Request.Code != c.code {
				t.Fatalf("status = %+v", st.Request)
			}
			if name == "wrong pin" && (res.AttemptsRemaining == nil || *res.AttemptsRemaining != 4) {
				t.Fatalf("attempts = %v", res.AttemptsRemaining)
			}
			if name == "locked" && (res.LockedUntil == nil || !res.LockedUntil.Equal(until)) {
				t.Fatalf("locked until = %v", res.LockedUntil)
			}
		})
	}
}

// One request at a time, including its switch; an unasked-for grant
// response is dropped and never reaches the executor.
func TestRequestSingleFlightAndUnaskedResponses(t *testing.T) {
	exec := &fakeExecutor{status: owned(), release: make(chan struct{})}
	svc, _ := newTestService(t, exec, func(req proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
		return &proto.OperatingModeGrantResponseData{RequestID: req.RequestID, Granted: true, Grant: grantFor(req, time.Now())}
	}, nil)
	if res := svc.Request(context.Background(), context.Background(), proto.OperatingModeVR, pinBytes("4826")); !res.Accepted {
		t.Fatalf("first = %+v", res)
	}
	waitState(t, svc, StateSwitch)
	second := pinBytes("4826")
	if res := svc.Request(context.Background(), context.Background(), proto.OperatingModeAPClubs, second); res.Code != CodeBusy || *second != [4]byte{} {
		t.Fatalf("second = %+v", res)
	}
	unasked := &proto.OperatingModeGrantResponseData{RequestID: uuid.NewString(), Granted: true, Grant: grantFor(proto.OperatingModeGrantRequestData{TargetMode: "vr", ListRevision: 7}, time.Now())}
	env, _ := proto.NewEnvelope(proto.MsgOperatingModeGrantResponse, uuid.Nil, nil, unasked)
	svc.HandleGrantResponse(context.Background(), env)
	close(exec.release)
	waitState(t, svc, StateDone)
	if calls, _ := exec.snapshot(); len(calls) != 1 {
		t.Fatalf("executor calls = %d", len(calls))
	}
	if res := svc.Request(context.Background(), context.Background(), proto.OperatingModeAPClubs, pinBytes("4826")); !res.Accepted {
		t.Fatalf("after switch = %+v", res)
	}
}

func TestStatusPickerRequired(t *testing.T) {
	for _, c := range []struct {
		status *proto.OperatingModeEnforcementStatus
		want   bool
	}{
		{nil, false},
		{&proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseNone, Current: "held"}, false},
		{&proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseAcquiring, Current: "held"}, true},
		{&proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseOwned, Current: "held"}, true},
		{&proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseOwned, Current: "vr"}, false},
		{&proto.OperatingModeEnforcementStatus{Phase: proto.OperatingModePhaseReleasing, Current: "held"}, false},
	} {
		svc := New(Options{Executor: &fakeExecutor{status: c.status}})
		if got := svc.Status().PickerRequired; got != c.want {
			t.Errorf("%+v: picker required = %v", c.status, got)
		}
	}
}

func allow(*http.Request) error { return nil }

func post(t *testing.T, h http.Handler, contentType, body string) (*httptest.ResponseRecorder, Result) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://readyfleet.local/v1/mode/request", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out Result
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// The pipe route accepts only the two mode names and a four-digit PIN.
func TestHandlerInputValidation(t *testing.T) {
	exec := &fakeExecutor{status: owned()}
	svc, c2 := newTestService(t, exec, func(req proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
		return &proto.OperatingModeGrantResponseData{RequestID: req.RequestID, Code: "wrong_pin"}
	}, nil)
	h := Handler(svc, allow, context.Background())
	for _, body := range []string{
		``, `{}`, `[]`, `null`,
		`{"target_mode":"vr"}`,
		`{"pin":"4826"}`,
		`{"target_mode":"held","pin":"4826"}`,
		`{"target_mode":"VR","pin":"4826"}`,
		`{"target_mode":"creative","pin":"4826"}`,
		`{"target_mode":"vr","pin":4826}`,
		`{"target_mode":"vr","pin":"482"}`,
		`{"target_mode":"vr","pin":"48266"}`,
		`{"target_mode":"vr","pin":"48a6"}`,
		`{"target_mode":"vr","pin":"\u0034826"}`,
		`{"target_mode":"vr","pin":"4826","command":"cmd.exe"}`,
		`{"target_mode":"vr","pin":"4826","path":"C:\\x.exe"}`,
		`{"target_mode":"vr","pin":"4826"} {"target_mode":"vr","pin":"4826"}`,
		`{"target_mode":"vr","pin":"4826"}` + strings.Repeat(" ", MaxRequestBytes),
	} {
		rec, _ := post(t, h, "application/json", body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%q = %d %s", body, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "4826") {
			t.Errorf("%q: response echoes input", body)
		}
	}
	if rec, _ := post(t, h, "text/plain", `{"target_mode":"vr","pin":"4826"}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content type = %d", rec.Code)
	}
	if len(c2.requests()) != 0 {
		t.Fatalf("invalid bodies reached the C2: %d", len(c2.requests()))
	}
	rec, out := post(t, h, "application/json; charset=utf-8", `{"target_mode":"ap_clubs","pin":"0427"}`)
	if seen := c2.requests(); rec.Code != http.StatusOK || out.Code != "wrong_pin" || len(seen) != 1 || seen[0].PIN != "0427" {
		t.Fatalf("valid = %d %+v %+v", rec.Code, out, seen)
	}

	// Other methods, paths and queries; and an unauthorised client.
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/v1/mode/request"},
		{http.MethodPost, "/v1/mode/status"},
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/mode/status?x=1"},
		{http.MethodDelete, "/v1/mode/status"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, "http://readyfleet.local"+c.target, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s = 200", c.method, c.target)
		}
	}
	denied := Handler(svc, func(*http.Request) error { return errors.New("wrong SID") }, context.Background())
	rec = httptest.NewRecorder()
	denied.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://readyfleet.local/v1/mode/status", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorised = %d", rec.Code)
	}
	if rec, _ := post(t, Handler(svc, nil, context.Background()), "application/json", `{"target_mode":"vr","pin":"4826"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("nil authorize = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://readyfleet.local/v1/mode/status", nil))
	var st Status
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.Protocol != ProtocolVersion || !st.Enforced {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
}

func waitReports(t *testing.T, c2 *fakeC2, n int) []proto.OperatingModeTransitionReport {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r := c2.transitionReports(); len(r) >= n {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d transition report(s), got %d", n, len(c2.transitionReports()))
	return nil
}

func TestLocalSwitchIsReportedToC2(t *testing.T) {
	exec := &fakeExecutor{status: owned()}
	var granted *proto.OperatingModeGrant
	svc, c2 := newTestService(t, exec, func(req proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
		granted = grantFor(req, time.Now())
		return &proto.OperatingModeGrantResponseData{RequestID: req.RequestID, Granted: true, Grant: granted}
	}, nil)
	c2.reportFails = 1 // the first send fails; the retry delivers it
	if res := svc.Request(context.Background(), context.Background(), proto.OperatingModeVR, pinBytes("4826")); !res.Accepted {
		t.Fatalf("result = %+v", res)
	}
	reps := waitReports(t, c2, 1)
	r := reps[0]
	if r.Outcome != proto.OperatingModeOutcomeSwitched || r.To != proto.OperatingModeVR || r.RequestID == "" {
		t.Fatalf("report = %+v", r)
	}
	// The fake executor echoes no grant fields; the service fills them in.
	if r.GrantID != granted.GrantID || r.Source != proto.OperatingModeGrantSourceLocalPIN || r.Actor != granted.Actor {
		t.Fatalf("report grant identity = %q %q %q", r.GrantID, r.Source, r.Actor)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "4826") {
		t.Fatal("report carries the PIN")
	}
}

func TestRefusedPINIsNotReportedAsTransition(t *testing.T) {
	exec := &fakeExecutor{status: owned()}
	svc, c2 := newTestService(t, exec, func(r proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
		return &proto.OperatingModeGrantResponseData{RequestID: r.RequestID, Code: "wrong_pin"}
	}, nil)
	svc.Request(context.Background(), context.Background(), proto.OperatingModeVR, pinBytes("1111"))
	time.Sleep(50 * time.Millisecond)
	if n := len(c2.transitionReports()); n != 0 {
		t.Fatalf("a refused PIN (no switch, no grant) produced %d transition report(s)", n)
	}
}
