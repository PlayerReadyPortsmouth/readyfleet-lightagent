package sessionindicator

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tri is a three-valued answer to a question about the screens.
//
// Unknown is a first-class value, not a synonym for No. Every probe here
// talks to something that may not be there — a session bus with no
// screen locker on it, a kernel without the DRM sysfs nodes, a Windows
// API that answers for a session the agent is not in. Collapsing "could
// not find out" into "no" would make an unreachable compositor look like
// a wide-awake unlocked desktop, and the skip rule would then behave as
// though it had checked when it had not. Unknown is reported as Unknown
// and the caller decides; see Decide, where Unknown means SHOW.
type Tri uint8

const (
	// Unknown means no probe could answer.
	Unknown Tri = iota
	// No means a probe answered, and the answer was no.
	No
	// Yes means a probe answered, and the answer was yes.
	Yes
)

func (t Tri) String() string {
	switch t {
	case No:
		return "no"
	case Yes:
		return "yes"
	default:
		return "unknown"
	}
}

// ScreenState is what the machine's screens are doing right now, as far
// as the agent can tell.
//
// Source names the probe that produced each answer, so the skip line in
// the log says which mechanism decided rather than only what it decided.
// Both may be empty when the answer is Unknown.
type ScreenState struct {
	// Locked is whether the session is locked — a lock screen, a
	// screensaver, a switched-away session.
	Locked Tri
	// LockedSource names what answered Locked (e.g.
	// "org.freedesktop.ScreenSaver", "logind LockedHint",
	// "WTSSessionInfoEx").
	LockedSource string

	// DisplaysOff is whether every display attached to this machine is
	// powered down (DPMS off / asleep).
	DisplaysOff Tri
	// DisplaysOffSource names what answered DisplaysOff.
	DisplaysOffSource string
}

// ErrSkipped is the sentinel behind every Skipped. Callers that only need
// to know "this was a deliberate skip, not a failure" match on it with
// errors.Is; callers that want to log the reason use errors.As on
// *Skipped.
var ErrSkipped = errors.New("session indicator skipped")

// Skipped is returned by Show when the indicator was DELIBERATELY not
// attempted, because there is nobody in front of the screen to show it
// to.
//
// This is emphatically not a failure. It is the difference between "the
// bar could not be put up" and "there was no point putting the bar up",
// and the whole reason the two are distinguishable types is that the
// require_session_indicator policy must react to the first and must NOT
// react to the second. A machine that is locked or asleep would refuse
// every session under that policy otherwise, which is the opposite of
// what the policy is for.
type Skipped struct {
	// Reason is a human sentence naming what was true, for the log.
	Reason string
	// State is the reading the decision was made from.
	State ScreenState
}

func (s *Skipped) Error() string {
	return "session indicator skipped: " + s.Reason
}

// Is makes errors.Is(err, ErrSkipped) work through any amount of wrapping.
func (s *Skipped) Is(target error) bool { return target == ErrSkipped }

// Decide says whether to put the bar up for a given reading of the
// screens.
//
// The rule, which is Aurora's ruling of 7 Sep 2026 and not a default
// arrived at by accident:
//
//	If a machine is locked or its displays are off, just skip the
//	indicator — for BYOD it requires approval anyway.
//
// The reasoning is that a BYOD connection from anyone other than the
// device's enrolled owner already puts an explicit consent prompt in
// front of the person at the machine (internal/consent,
// internal/desktopnotify), so that person has been told by a route that
// does not depend on a bar being drawable; and the owner connecting to
// their own device is unattended by design and needs no indicator at all.
// A locked or sleeping screen has nobody reading it, so drawing to it
// buys nothing — and on Linux it would mean holding a layer-shell surface
// and an exclusive zone open against a compositor that has powered the
// outputs down.
//
// Skipping never refuses the session. That is the other half of the
// ruling: the caller carries on, and logs that it skipped and why.
//
// Unknown means SHOW. A probe that could not answer must not be able to
// silence the indicator — the conservative direction here is to draw the
// bar, because an unnecessary bar costs a band of pixels and a missing
// one costs the whole guarantee.
func Decide(s ScreenState) (skip bool, reason string) {
	var why []string
	if s.Locked == Yes {
		why = append(why, describe("the session is locked", s.LockedSource))
	}
	if s.DisplaysOff == Yes {
		why = append(why, describe("the displays are asleep", s.DisplaysOffSource))
	}
	if len(why) == 0 {
		return false, ""
	}
	return true, strings.Join(why, " and ")
}

func describe(what, source string) string {
	if source == "" {
		return what
	}
	return fmt.Sprintf("%s (%s)", what, source)
}

// skippedFor builds the error Show returns for a skip.
func skippedFor(reason string, s ScreenState) error {
	return &Skipped{Reason: reason, State: s}
}

// triOf turns a probe's definite yes/no into a Tri. Probes that could not
// answer return Unknown themselves rather than going through this.
func triOf(b bool) Tri {
	if b {
		return Yes
	}
	return No
}

// callTimeout bounds a single D-Bus round trip.
const callTimeout = 2 * time.Second

// probeTimeout bounds the WHOLE screen-state read, however many calls it
// makes and wherever it wedges.
//
// This is not belt-and-braces over callTimeout. Show is reached from
// sessionIndicatorDriver.Preflight, which sits on the start_solarbeam
// command path: a probe that blocks blocks a support session from
// starting. Only the method calls themselves take a context — the
// connect/auth/Hello handshake on a private bus connection does not — so
// the bound has to be outside all of it.
const probeTimeout = 3 * time.Second

// readScreenStateBounded runs probe with a hard deadline, and reports
// Unknown if it does not finish in time.
//
// A timed-out probe deliberately leaks its goroutine rather than being
// cancelled: there is nothing to cancel it with, and abandoning one
// blocked D-Bus handshake that will eventually error out is far cheaper
// than holding a session-start command open behind it. The channel is
// buffered so the abandoned goroutine can still finish its send and exit.
func readScreenStateBounded(probe func() ScreenState, timeout time.Duration) ScreenState {
	ch := make(chan ScreenState, 1)
	go func() { ch <- probe() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s := <-ch:
		return s
	case <-timer.C:
		return ScreenState{}
	}
}
