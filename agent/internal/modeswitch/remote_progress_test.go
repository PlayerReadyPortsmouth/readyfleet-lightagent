package modeswitch

import (
	"context"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

type progressExecutor struct {
	fakeExecutor
	active *proto.OperatingModeTransitionReport
}

func (e *progressExecutor) ModeStatus() (*proto.OperatingModeEnforcementStatus, *proto.OperatingModeTransitionReport) {
	return e.EnforcementStatus(), e.active
}

func TestRemoteTransitionProgressAndResultReachPicker(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	e := &progressExecutor{fakeExecutor: fakeExecutor{status: owned()}, active: &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, From: "ap_clubs", To: "vr", StartedAt: now}}
	e.active.Steps = []proto.OperatingModeTransitionStepResult{{ItemID: "synthesis", Action: "open", Result: "running"}}
	e.status.Journal = "in_progress"
	s := New(Options{Executor: e, Now: func() time.Time { return now }})
	s.record(RequestView{State: StateDone, RequestID: "old-local"})
	st := s.Status()
	if !st.PickerRequired || st.Request.State != StateSwitch || st.Request.RequestID != "remote" || st.Request.TargetMode != "vr" || st.Request.Source != proto.OperatingModeGrantSourceRemoteStaff {
		t.Fatalf("remote progress missing: %+v", st)
	}
	if st.Request.Report == nil || len(st.Request.Report.Steps) != 1 || st.Request.Report.Steps[0].Result != "running" || !st.Request.Report.StartedAt.Equal(now) {
		t.Fatalf("active report omitted from local status: %+v", st.Request)
	}
	// An older/stale picker must not request a second PIN grant during remote work.
	pin := [4]byte{'1', '2', '3', '4'}
	if r := s.Request(context.Background(), context.Background(), "ap_clubs", &pin); r.Code != CodeBusy || pin != [4]byte{} {
		t.Fatalf("parallel local request accepted: %+v", r)
	}
	for _, outcome := range []string{proto.OperatingModeOutcomeSwitched, proto.OperatingModeOutcomeFailedHeld, proto.OperatingModeOutcomeRefused} {
		e.active = nil
		e.status.Journal = "idle"
		e.status.LastTransition = &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, To: "vr", Outcome: outcome, FinishedAt: now.Add(time.Second)}
		st = s.Status()
		if st.Request.State != StateDone || st.Request.Report == nil || st.Request.Report.Outcome != outcome || st.Request.RequestID != "remote" {
			t.Fatalf("remote result missing: %+v", st)
		}
	}
	now = now.Add(2 * time.Second)
	s.record(RequestView{State: StateChecking, RequestID: "new-local"})
	if st = s.Status(); st.Request.RequestID != "new-local" {
		t.Fatalf("old remote report replaced newer local request: %+v", st)
	}
}

func TestLateOldPINRefusalDoesNotHideRemoteCompletion(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	e := &progressExecutor{fakeExecutor: fakeExecutor{status: owned()}}
	s := New(Options{Executor: e, Now: func() time.Time { return now }})
	s.record(RequestView{State: StateChecking, Source: proto.OperatingModeGrantSourceLocalPIN, RequestID: "old-pin"})
	now = now.Add(time.Second)
	e.active = &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, To: "vr", StartedAt: now}
	if st := s.Status(); st.Request.RequestID != "remote" || st.Request.State != StateSwitch {
		t.Fatalf("remote switch not adopted: %+v", st)
	}
	now = now.Add(time.Second)
	e.active = nil
	e.status.LastTransition = &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, To: "vr", Outcome: proto.OperatingModeOutcomeSwitched, FinishedAt: now}
	now = now.Add(time.Second)
	s.refuse(RequestView{Source: proto.OperatingModeGrantSourceLocalPIN, RequestID: "old-pin"}, CodeNoAnswer, "PIN request timed out")
	if st := s.Status(); st.Request.RequestID != "remote" || st.Request.State != StateDone {
		t.Fatalf("late refusal hid remote completion: %+v", st)
	}
	// A genuinely new request must not be replaced by the completed switch.
	s.record(RequestView{State: StateChecking, Source: proto.OperatingModeGrantSourceLocalPIN, RequestID: "new-pin"})
	if st := s.Status(); st.Request.RequestID != "new-pin" || st.Request.State != StateChecking {
		t.Fatalf("old remote completion replaced new request: %+v", st)
	}
}

func TestPINGrantDoesNotDispatchAfterRemoteWorkSupersedesIt(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "remote still running"
		if completed {
			name = "remote already finished"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
			e := &progressExecutor{fakeExecutor: fakeExecutor{status: owned()}}
			c2 := &fakeC2{t: t}
			s := New(Options{MachineID: testMachine, Executor: e, Send: c2.send, Now: func() time.Time { return now }})
			c2.svc = s
			c2.answer = func(req proto.OperatingModeGrantRequestData) *proto.OperatingModeGrantResponseData {
				now = now.Add(time.Second)
				e.active = &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, To: "vr", StartedAt: now}
				e.status.Journal = "in_progress"
				if completed {
					now = now.Add(time.Second)
					e.active = nil
					e.status.Journal = "idle"
					e.status.LastTransition = &proto.OperatingModeTransitionReport{RequestID: "remote", Source: proto.OperatingModeGrantSourceRemoteStaff, To: "vr", Outcome: proto.OperatingModeOutcomeSwitched, FinishedAt: now}
				}
				return &proto.OperatingModeGrantResponseData{RequestID: req.RequestID, Granted: true, Grant: grantFor(req, now)}
			}
			pin := [4]byte{'1', '2', '3', '4'}
			r := s.Request(context.Background(), context.Background(), "ap_clubs", &pin)
			if r.Accepted || r.Code != CodeBusy || pin != [4]byte{} {
				t.Fatalf("superseded PIN grant accepted: %+v", r)
			}
			if calls, _ := e.snapshot(); len(calls) != 0 {
				t.Fatalf("superseded local switch dispatched: %+v", calls)
			}
			if st := s.Status(); st.Request.RequestID != "remote" {
				t.Fatalf("old PIN refusal replaced remote progress/result: %+v", st)
			}
		})
	}
}
