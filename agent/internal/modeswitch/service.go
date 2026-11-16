// Package modeswitch is the SYSTEM service's half of the venue mode picker
// (docs/superpowers/plans/2026-09-14-operating-mode-switcher.md, step 3b).
//
// The venue companion, running as the CYP user, can ask for exactly one
// thing: switch to ap_clubs or vr with a four-digit venue PIN. The service
// never checks the PIN itself. It forwards it to the C2 as
// MsgOperatingModeGrantRequest over the agent's mTLS socket, waits for the
// matching MsgOperatingModeGrantResponse, and hands a granted
// OperatingModeTransitionRequest to the enforce build's transition executor
// with source local_pin. A grant is accepted ONLY as the response to a request
// this service sent and has not yet seen answered; the companion never sees
// or supplies one.
//
// The PIN is held as bytes, wiped as soon as the request is sent, and never
// logged. It unavoidably becomes a Go string inside the wire payload
// (proto.OperatingModeGrantRequestData.PIN) and passes through JSON and
// WebSocket buffers the garbage collector owns; those copies are not wiped.
//
// Before contacting the C2 the service refuses locally when the executor
// reports no complete enforcement (nothing to switch, and no attempt should
// be spent) or the controller is not connected. One request runs at a time.
package modeswitch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Executor is the enforce build's transition entry point, called through
// this interface so the picker build compiles and tests without it.
type Executor interface {
	// EnforcementStatus is the agent's current enforcement state, or nil when
	// this agent has no enforce build or the machine was never enforced.
	EnforcementStatus() *proto.OperatingModeEnforcementStatus
	// Transition runs one switch. source is the path the grant arrived by;
	// the executor re-checks the grant (proto.CheckOperatingModeGrant plus
	// its persisted consumed-grant store) before any change.
	Transition(ctx context.Context, req proto.OperatingModeTransitionRequest, source string) proto.OperatingModeTransitionReport
}

// Unavailable is the Executor used until the enforce build is wired in: it
// reports no enforcement and refuses every transition.
type Unavailable struct{}

func (Unavailable) EnforcementStatus() *proto.OperatingModeEnforcementStatus { return nil }
func (Unavailable) Transition(_ context.Context, req proto.OperatingModeTransitionRequest, source string) proto.OperatingModeTransitionReport {
	now := time.Now().UTC()
	return proto.OperatingModeTransitionReport{RequestID: req.RequestID, Source: source, Outcome: proto.OperatingModeOutcomeRefused,
		Code: "not_enforced", Reason: "Mode switching is not built into this ReadyFleet agent.", Steps: []proto.OperatingModeTransitionStepResult{},
		StartedAt: now, FinishedAt: now}
}

// Request states reported to the companion.
const (
	StateIdle     = "idle"
	StateChecking = "checking_pin"
	StateSwitch   = "switching"
	StateDone     = "done"
	StateRefused  = "refused"
)

// Local refusal codes (the C2's codes pass through unchanged).
const (
	CodeNotEnforced  = proto.OperatingModeGrantCodeNotEnforced
	CodeOffline      = "offline"
	CodeNoAnswer     = "no_answer"
	CodeBusy         = "request_in_progress"
	CodeGrantInvalid = "grant_invalid"
)

// RequestView is the last picker request, as the companion may see it. It
// never carries the PIN or the grant.
type RequestView struct {
	State             string     `json:"state"`
	Source            string     `json:"source,omitempty"`
	RequestID         string     `json:"requestId,omitempty"`
	TargetMode        string     `json:"targetMode,omitempty"`
	Code              string     `json:"code,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	AttemptsRemaining *int       `json:"attemptsRemaining,omitempty"`
	LockedUntil       *time.Time `json:"lockedUntil,omitempty"`
	// While switching, Report is a local snapshot of completed actions plus
	// one running step; it is not a completed controller transition result.
	Report    *proto.OperatingModeTransitionReport `json:"report,omitempty"`
	UpdatedAt time.Time                            `json:"updatedAt"`
	startedAt time.Time                            // ordering survives late grant replies and refusals
}

// Status is GET /v1/mode/status.
type Status struct {
	Protocol       int       `json:"protocol"`
	ObservedAt     time.Time `json:"observedAt"`
	Connected      bool      `json:"connected"`
	ConnectedSince time.Time `json:"connectedSince"`
	// Enforced: the executor owns every route (phase owned). Only then can a
	// switch be requested.
	Enforced bool   `json:"enforced"`
	Phase    string `json:"phase"`
	// Current is a mode id or "held"; PickerRequired is Enforced && held.
	Current        string      `json:"current"`
	HeldReason     string      `json:"heldReason,omitempty"`
	PickerRequired bool        `json:"pickerRequired"`
	Pending        []string    `json:"pending,omitempty"`
	Request        RequestView `json:"request"`
}

// ProtocolVersion of the mode pipe.
const ProtocolVersion = 1

// Connection reports the agent's controller connection.
type Connection func() (connected bool, since time.Time)

// Options configure a Service.
type Options struct {
	MachineID  string
	Send       func(ctx context.Context, env proto.Envelope) error
	Connection Connection
	Executor   Executor
	Logger     *slog.Logger
	// AnswerTimeout bounds the wait for the C2 (default 10 s).
	AnswerTimeout time.Duration
	// ReportRetry is the wait before each retry of sending a finished local
	// switch's report to the C2 (default 5s, 15s, 30s, 60s). A report still
	// unsent after that reaches the C2 as enforcement.last_transition in the
	// next apply_operating_mode report.
	ReportRetry []time.Duration
	Now         func() time.Time
}

// Service is safe for concurrent use.
type Service struct {
	opts Options

	flight sync.Mutex // one picker request at a time, including its switch

	mu      sync.Mutex
	pending map[string]chan proto.OperatingModeGrantResponseData
	last    RequestView
}

// New returns a Service. A nil Executor is Unavailable.
func New(opts Options) *Service {
	if opts.Executor == nil {
		opts.Executor = Unavailable{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.AnswerTimeout <= 0 {
		opts.AnswerTimeout = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReportRetry == nil {
		opts.ReportRetry = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	}
	return &Service{opts: opts, pending: map[string]chan proto.OperatingModeGrantResponseData{}, last: RequestView{State: StateIdle}}
}

// snapshotExecutor is implemented by the managed agent so progress and state
// are read together, without waiting for its long-running transition lock.
type snapshotExecutor interface {
	ModeStatus() (*proto.OperatingModeEnforcementStatus, *proto.OperatingModeTransitionReport)
}

func (s *Service) modeStatus() (*proto.OperatingModeEnforcementStatus, *proto.OperatingModeTransitionReport) {
	if e, ok := s.opts.Executor.(snapshotExecutor); ok {
		return e.ModeStatus()
	}
	return s.opts.Executor.EnforcementStatus(), nil
}

// Status is the current view for the companion.
func (s *Service) Status() Status {
	st := Status{Protocol: ProtocolVersion, ObservedAt: s.opts.Now().UTC(), Phase: proto.OperatingModePhaseNone, Current: proto.OperatingModeHeld}
	if s.opts.Connection != nil {
		st.Connected, st.ConnectedSince = s.opts.Connection()
	}
	es, active := s.modeStatus()
	if es != nil {
		st.Phase, st.HeldReason = es.Phase, es.HeldReason
		if es.Current != "" {
			st.Current = es.Current
		}
		st.Enforced = es.Phase == proto.OperatingModePhaseOwned
		st.Pending = append([]string(nil), es.Pending...)
		if len(st.Pending) > 16 {
			st.Pending = st.Pending[:16]
		}
	}
	// Owned routes but not yet complete (acquiring/partial) still hold the
	// desktop: the picker shows "Getting this PC ready" until owned.
	holding := st.Phase == proto.OperatingModePhaseOwned || st.Phase == proto.OperatingModePhaseAcquiring || st.Phase == proto.OperatingModePhasePartial
	st.PickerRequired = holding && st.Current == proto.OperatingModeHeld
	s.mu.Lock()
	st.Request = s.last
	s.mu.Unlock()
	if active != nil && active.RequestID != "" && proto.IsPickerOperatingMode(active.To) {
		st.Request = RequestView{State: StateSwitch, Source: active.Source, RequestID: active.RequestID, TargetMode: active.To, Report: active, UpdatedAt: active.StartedAt}
	} else if es != nil && es.LastTransition != nil {
		r := es.LastTransition
		// A late reply to an older PIN request must not hide the remote
		// completion. A PIN request begun after completion still wins.
		if r.RequestID != "" && proto.IsPickerOperatingMode(r.To) && r.FinishedAt.After(st.Request.startedAt) {
			st.Request = RequestView{State: StateDone, Source: r.Source, RequestID: r.RequestID, TargetMode: r.To, Report: r, Code: r.Code, Reason: r.Reason, UpdatedAt: r.FinishedAt}
		}
	}
	return st
}

// Result is the immediate answer to a picker request.
type Result struct {
	Accepted          bool       `json:"accepted"`
	RequestID         string     `json:"requestId,omitempty"`
	Code              string     `json:"code,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	AttemptsRemaining *int       `json:"attemptsRemaining,omitempty"`
	LockedUntil       *time.Time `json:"lockedUntil,omitempty"`
}

func (s *Service) record(v RequestView) {
	v.UpdatedAt = s.opts.Now().UTC()
	s.mu.Lock()
	if v.startedAt.IsZero() {
		v.startedAt = v.UpdatedAt
		if v.RequestID != "" && v.RequestID == s.last.RequestID && !s.last.startedAt.IsZero() {
			v.startedAt = s.last.startedAt
		}
	}
	s.last = v
	s.mu.Unlock()
}

// Request asks the C2 for a grant and, if granted, starts the switch in the
// background (ctx for the switch is runCtx, which outlives the pipe request).
// pin is wiped before Request returns, whatever the outcome.
func (s *Service) Request(ctx, runCtx context.Context, targetMode string, pin *[proto.OperatingModePINDigits]byte) Result {
	defer clear(pin[:])
	if !proto.IsPickerOperatingMode(targetMode) || !digits(pin[:]) {
		return Result{Code: proto.OperatingModeGrantCodeInvalidRequest, Reason: "Choose AP / Clubs or VR and enter four digits."}
	}
	if !s.flight.TryLock() {
		return Result{Code: CodeBusy, Reason: "A switch is already in progress on this PC."}
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.flight.Unlock()
		}
	}()

	startedAt := s.opts.Now().UTC()
	es, active := s.modeStatus()
	if active != nil || (es != nil && es.Journal == "in_progress") {
		return Result{Code: CodeBusy, Reason: "A switch is already in progress on this PC."}
	}
	if es == nil || es.Phase != proto.OperatingModePhaseOwned || es.ListRevision < 1 {
		return s.refuse(RequestView{TargetMode: targetMode}, CodeNotEnforced, "Mode switching isn't turned on for this PC.")
	}
	if s.opts.Connection != nil {
		if connected, _ := s.opts.Connection(); !connected {
			return s.refuse(RequestView{TargetMode: targetMode}, CodeOffline, "Can't reach ReadyFleet, so the PIN can't be checked.")
		}
	}

	previousTransitionID := ""
	var previousTransitionFinishedAt time.Time
	if es.LastTransition != nil {
		previousTransitionID, previousTransitionFinishedAt = es.LastTransition.RequestID, es.LastTransition.FinishedAt
	}
	requestID := uuid.NewString()
	answer := make(chan proto.OperatingModeGrantResponseData, 1)
	s.mu.Lock()
	s.pending[requestID] = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, requestID)
		s.mu.Unlock()
	}()
	view := RequestView{State: StateChecking, Source: proto.OperatingModeGrantSourceLocalPIN, RequestID: requestID, TargetMode: targetMode, startedAt: startedAt}
	s.record(view)

	env, err := proto.NewEnvelope(proto.MsgOperatingModeGrantRequest, uuid.Nil, nil, proto.OperatingModeGrantRequestData{
		RequestID: requestID, TargetMode: targetMode, PIN: string(pin[:]), ListRevision: es.ListRevision,
	})
	clear(pin[:])
	if err != nil {
		return s.refuse(view, CodeNoAnswer, "The PIN couldn't be sent. Try again.")
	}
	sendErr := s.opts.Send(ctx, env)
	clear(env.Data)
	if sendErr != nil {
		return s.refuse(view, CodeOffline, "Can't reach ReadyFleet, so the PIN can't be checked.")
	}

	var resp proto.OperatingModeGrantResponseData
	timer := time.NewTimer(s.opts.AnswerTimeout)
	defer timer.Stop()
	select {
	case resp = <-answer:
	case <-timer.C:
		return s.refuse(view, CodeNoAnswer, "ReadyFleet didn't answer. Check the connection and try again.")
	case <-ctx.Done():
		return s.refuse(view, CodeNoAnswer, "The request was cancelled. Try again.")
	}

	if !resp.Granted {
		view.AttemptsRemaining, view.LockedUntil = resp.AttemptsRemaining, resp.LockedUntil
		code, reason := resp.Code, resp.Reason
		if code == "" {
			code = proto.OperatingModeGrantCodeError
		}
		if reason == "" || len(reason) > 300 || strings.ContainsAny(reason, "\r\n\x00") {
			reason = "ReadyFleet did not allow the switch."
		}
		r := s.refuse(view, code, reason)
		r.AttemptsRemaining, r.LockedUntil = resp.AttemptsRemaining, resp.LockedUntil
		return r
	}
	if err := s.checkGrant(resp.Grant, es, targetMode); err != nil {
		s.opts.Logger.Warn("mode picker: refused a grant response", "request_id", requestID, "err", err)
		return s.refuse(view, CodeGrantInvalid, "ReadyFleet's answer didn't match this PC. Try again.")
	}

	// A ReadyApp switch can run while the PIN response is in flight. That
	// earlier local choice must not switch the PC back after remote work.
	latest, active := s.modeStatus()
	if active != nil || (latest != nil && latest.Journal == "in_progress") {
		return s.refuse(view, CodeBusy, "A switch was started in ReadyApp. Wait for it to finish, then choose again.")
	}
	if latest == nil || latest.Phase != proto.OperatingModePhaseOwned || latest.ListRevision != es.ListRevision {
		return s.refuse(view, CodeNotEnforced, "Mode settings changed while the PIN was checked. Choose again.")
	}
	if r := latest.LastTransition; r != nil && r.Source == proto.OperatingModeGrantSourceRemoteStaff &&
		(r.RequestID != previousTransitionID || !r.FinishedAt.Equal(previousTransitionFinishedAt)) {
		return s.refuse(view, CodeBusy, "A switch was completed in ReadyApp. Choose again to request another mode.")
	}

	view.State = StateSwitch
	s.record(view)
	handedOff = true
	go func() {
		defer s.flight.Unlock()
		report := s.opts.Executor.Transition(runCtx, proto.OperatingModeTransitionRequest{RequestID: requestID, Grant: resp.Grant}, proto.OperatingModeGrantSourceLocalPIN)
		s.opts.Logger.Info("mode picker: switch finished", "request_id", requestID, "to", targetMode, "outcome", report.Outcome, "code", report.Code)
		done := view
		done.State = StateDone
		done.Report = &report
		done.Code, done.Reason = report.Code, report.Reason
		s.record(done)
		s.reportToC2(runCtx, report, resp.Grant)
	}()
	return Result{Accepted: true, RequestID: requestID}
}

func digits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(b) == proto.OperatingModePINDigits
}

func (s *Service) refuse(view RequestView, code, reason string) Result {
	view.State, view.Code, view.Reason = StateRefused, code, reason
	s.record(view)
	return Result{RequestID: view.RequestID, Code: code, Reason: reason}
}

// checkGrant is the service's own pre-check before the executor's full one:
// the grant must be for this machine, this venue and list revision, the mode
// that was asked for, arrive as local_pin, and be within its lifetime.
func (s *Service) checkGrant(g *proto.OperatingModeGrant, es *proto.OperatingModeEnforcementStatus, targetMode string) error {
	if g == nil {
		return errors.New("granted response carries no grant")
	}
	if g.TargetMode != targetMode {
		return errors.New("grant is for a different mode")
	}
	return proto.CheckOperatingModeGrant(g, proto.OperatingModeGrantExpectation{
		MachineID: s.opts.MachineID, VenueID: es.VenueID, ListRevision: es.ListRevision,
		Modes: []string{proto.OperatingModeAPClubs, proto.OperatingModeVR}, Source: proto.OperatingModeGrantSourceLocalPIN, Now: s.opts.Now(),
	})
}

// HandleGrantResponse routes a MsgOperatingModeGrantResponse to the request
// waiting for it. A response to anything this service is not waiting for
// is dropped: that is what stops a grant arriving unasked.
func (s *Service) HandleGrantResponse(_ context.Context, env proto.Envelope) {
	var resp proto.OperatingModeGrantResponseData
	if err := env.DecodeData(&resp); err != nil {
		s.opts.Logger.Warn("mode picker: undecodable grant response")
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[resp.RequestID]
	if ok {
		delete(s.pending, resp.RequestID)
	}
	s.mu.Unlock()
	if !ok {
		s.opts.Logger.Warn("mode picker: dropped an unexpected grant response", "request_id", resp.RequestID)
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// reportToC2 sends a finished local switch to the C2 as
// MsgOperatingModeTransitionReport, so Fleet's current mode and transition
// history include switches made at the PC. The grant's identity is filled in
// when the executor refused before echoing it (the C2 records a local report
// only with a grant id, which is also how it drops the duplicate that arrives
// later inside enforcement.last_transition). Sending is best-effort with a few
// retries; the switch has already happened either way.
func (s *Service) reportToC2(ctx context.Context, report proto.OperatingModeTransitionReport, g *proto.OperatingModeGrant) {
	if report.GrantID == "" {
		report.GrantID = g.GrantID
	}
	if report.Source == "" {
		report.Source = proto.OperatingModeGrantSourceLocalPIN
	}
	if report.Actor == "" {
		report.Actor = g.Actor
	}
	if report.To == "" {
		report.To = g.TargetMode
	}
	for attempt := 0; ; attempt++ {
		env, err := proto.NewEnvelope(proto.MsgOperatingModeTransitionReport, uuid.Nil, nil, report)
		if err != nil {
			return
		}
		if err := s.opts.Send(ctx, env); err == nil {
			return
		}
		if attempt >= len(s.opts.ReportRetry) {
			s.opts.Logger.Warn("mode picker: could not report the switch to ReadyFleet; it will follow in the next enforcement report",
				"request_id", report.RequestID)
			return
		}
		t := time.NewTimer(s.opts.ReportRetry[attempt])
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
