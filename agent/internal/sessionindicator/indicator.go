package sessionindicator

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ReserveHeight is the height, in LOGICAL pixels, of the band along the
// top edge of the screen that the engine keeps out of the capture and
// that this package draws the indicator into.
//
// It is the whole contract between the two repos: the engine's
// `capture_reserve_top` default (solarbeam-engine-v2,
// src/platform/linux/kwingrab.cpp) is this number. Raising it here alone
// puts part of the bar back inside the streamed region, so the two move
// together or not at all.
//
// 40 is chosen to be tall enough for one line of large, unmissable text
// at 1x, and small enough that giving it up as an exclusive zone costs a
// desktop almost nothing.
const ReserveHeight = 40

// ErrUnsupported is returned by New on a platform with no implementation.
var ErrUnsupported = errors.New("session indicator: unsupported platform")

// Notice is what the bar says. Viewer may be empty: the LiveKit bridge's
// /status tells the agent HOW MANY remote participants are watching but
// never who, so a name is only present when the C2 pushed one (see the
// session_started notification kind in cmd/lightagent).
//
// Message, when set, replaces the whole line (the managed venue companion's
// fixed "Staff are viewing this screen"); it is a caller constant, never
// wire input.
type Notice struct {
	Viewer  string
	Message string
}

// StaffViewingMessage is the managed-PC bar text.
const StaffViewingMessage = "Staff are viewing this screen"

// Text is the line drawn on the bar.
func (n Notice) Text() string {
	if n.Message != "" {
		return n.Message
	}
	if v := sanitiseViewer(n.Viewer); v != "" {
		return "Your screen is being viewed remotely - " + v
	}
	return "Your screen is being viewed remotely"
}

// Indicator is the on-screen "you are being watched" bar.
//
// Show is idempotent for a given Notice: calling it repeatedly with the
// same Notice (which is what the poll loop does) leaves the existing bar
// alone rather than tearing it down and rebuilding it every tick.
type Indicator interface {
	// Show puts the bar up, or updates its text if it is already up with
	// a different Notice. An error means the bar is NOT on screen —
	// callers must treat that as loudly as their policy requires (see the
	// package doc's degradation section).
	//
	// TWO KINDS OF ERROR, and callers must tell them apart. A *Skipped
	// (errors.Is(err, ErrSkipped)) means the bar was deliberately not
	// attempted because the machine is locked or its displays are
	// asleep — nothing is wrong, and no policy should fire. Anything
	// else means the bar could not be put up, which is the condition
	// require_session_indicator exists for.
	Show(Notice) error
	// Hide takes the bar down. Safe to call when nothing is shown.
	Hide() error
	// Close releases every resource. The Indicator is unusable after.
	Close() error
}

// bar is one live on-screen indicator owned by a platform backend.
type bar interface {
	// Done is closed when the bar stops existing for a reason other than
	// a Close call on it — the compositor sent zwlr_layer_surface_v1
	// .closed, the Wayland connection dropped, the window was destroyed.
	// The supervisor treats that as "put it back".
	Done() <-chan struct{}
	// Close tears the bar down. Idempotent.
	Close()
}

// startBarFunc creates a bar showing n and returns once it is actually on
// screen, or returns an error if it could not be created. This is the one
// seam between the platform-neutral supervisor below and the compositor /
// Win32 work, so tests never touch either.
type startBarFunc func(n Notice) (bar, error)

// retryDelays is the backoff used when a bar dies while it is still
// wanted. It escalates and then holds: a compositor that keeps closing
// the surface should not be hammered, but the indicator must keep trying
// forever, because giving up silently is precisely the failure mode this
// package exists to remove.
var retryDelays = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}

// supervisor is the platform-neutral half of every Indicator: it owns
// "what should be on screen", re-asserts the bar if something tears it
// down, and guarantees the first Show reports a real failure synchronously
// rather than swallowing it into a retry loop.
type supervisor struct {
	start startBarFunc
	log   *slog.Logger
	// screenState reads whether anyone is in front of the screen. The
	// second seam in this package, alongside startBarFunc, so the skip
	// rule is exercised by ordinary table tests on every platform rather
	// than only on a machine somebody can lock.
	screenState func() ScreenState

	mu      sync.Mutex
	closed  bool
	wanted  bool
	notice  Notice
	current bar
	// generation invalidates the watcher goroutine of a bar we replaced,
	// so a torn-down-on-purpose bar never triggers a re-assert.
	generation uint64
	// sleep is time.Sleep, swapped by tests so backoff costs nothing.
	sleep func(time.Duration)
}

// newSupervisor takes the screen-state probe explicitly rather than
// defaulting to the platform one. A default would mean a test that
// forgot to override it silently ran the REAL probe against the
// developer's own desktop, and would then pass or fail depending on
// whether that desktop happened to be locked — which is precisely the
// kind of test this seam exists to make impossible.
func newSupervisor(start startBarFunc, screenState func() ScreenState, log *slog.Logger) *supervisor {
	if log == nil {
		log = slog.Default()
	}
	if screenState == nil {
		screenState = func() ScreenState { return ScreenState{} }
	}
	return &supervisor{start: start, screenState: screenState, log: log, sleep: time.Sleep}
}

// platformScreenState is the real probe, bounded, as New() wires it.
func platformScreenState() ScreenState {
	return readScreenStateBounded(readScreenState, probeTimeout)
}

func (s *supervisor) Show(n Notice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session indicator: closed")
	}
	if s.wanted && s.current != nil && s.notice == n {
		return nil // already saying exactly this
	}

	// The skip rule, and its position in this function is the whole of
	// it. It sits AFTER the idempotence check, so a bar that is already
	// up when the machine locks stays up — taking it down would mean the
	// person unlocking sees an unannounced session for up to one poll
	// interval, and the bar costs nothing while it stands. And it sits
	// BEFORE stopLocked, so a skip changes no state at all: the next poll
	// re-reads the screens, and the bar appears within one tick of the
	// person coming back. See Decide.
	//
	// The probe runs holding s.mu, which is a deliberate trade: it is
	// bounded at probeTimeout by construction, and the alternative —
	// dropping the lock to probe and retaking it — buys a TOCTOU race
	// between the poll loop and a concurrent session start in exchange
	// for three seconds nobody is waiting on.
	st := s.screenState()
	if skip, reason := Decide(st); skip {
		s.log.Info("session indicator: not shown, because there is nobody at the screen to show it to",
			"phase", "session_indicator_skipped", "reason", reason,
			"locked", st.Locked.String(), "displays_off", st.DisplaysOff.String())
		return skippedFor(reason, st)
	}

	s.stopLocked()
	s.wanted = true
	s.notice = n

	b, err := s.start(n)
	if err != nil {
		// Leave wanted=false: a Show that failed must not leave a
		// half-armed supervisor that later succeeds silently. The caller
		// gets the error and decides, per policy, what it means.
		s.wanted = false
		return fmt.Errorf("show session indicator: %w", err)
	}
	s.generation++
	s.current = b
	go s.watch(b, s.generation, n)
	return nil
}

func (s *supervisor) Hide() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return nil
}

func (s *supervisor) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.closed = true
	return nil
}

// stopLocked tears down whatever is on screen and makes its watcher a
// no-op. Caller holds s.mu.
func (s *supervisor) stopLocked() {
	s.wanted = false
	s.generation++
	if s.current != nil {
		s.current.Close()
		s.current = nil
	}
}

// watch re-asserts the bar if it dies while still wanted. It is the
// "cannot be dismissed" guarantee: a compositor closing the surface, or
// a crash of whatever hosts it, gets the bar back rather than leaving the
// session silently unannounced.
func (s *supervisor) watch(b bar, gen uint64, n Notice) {
	<-b.Done()
	attempt := 0
	for {
		s.mu.Lock()
		if s.closed || !s.wanted || s.generation != gen {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		delay := retryDelays[min(attempt, len(retryDelays)-1)]
		attempt++
		s.log.Warn("session indicator: bar went away while a session is live; re-asserting",
			"phase", "session_indicator_reassert", "attempt", attempt, "retry_in", delay)
		s.sleep(delay)

		s.mu.Lock()
		if s.closed || !s.wanted || s.generation != gen {
			s.mu.Unlock()
			return
		}
		// The same skip rule applies to a re-assert, because a re-assert
		// is an attempt to show. Without this, a bar the compositor tore
		// down as the machine locked would be rebuilt against a screen
		// nobody is looking at, and every failure to do so would be
		// logged at Error. `wanted` stays true, so the loop simply
		// re-checks after the next backoff and the bar returns when the
		// person does.
		st := s.screenState()
		if skip, reason := Decide(st); skip {
			s.log.Info("session indicator: not re-asserting the bar, because there is nobody at the screen to show it to",
				"phase", "session_indicator_skipped", "reason", reason,
				"locked", st.Locked.String(), "displays_off", st.DisplaysOff.String())
			s.mu.Unlock()
			continue
		}
		nb, err := s.start(n)
		if err != nil {
			s.log.Error("session indicator: could not re-assert the bar; the local person may not know a session is live",
				"phase", "session_indicator_reassert_failed", "err", err)
			s.mu.Unlock()
			continue
		}
		s.generation++
		gen = s.generation
		s.current = nb
		s.mu.Unlock()

		b = nb
		attempt = 0
		<-b.Done()
	}
}

// sanitiseViewer reduces a viewer name to the printable ASCII the bitmap
// font can actually draw, and bounds its length. The name arrives from
// the C2 (which fills it from the authenticated actor, never from a
// request body), but it is still remote-supplied text being rendered onto
// the local person's screen: an unbounded string would push the real
// warning off the edge of the bar, which is the one thing the bar must
// never lose.
func sanitiseViewer(v string) string {
	const maxLen = 40
	out := make([]rune, 0, maxLen)
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			r = ' '
		}
		out = append(out, r)
		if len(out) == maxLen {
			break
		}
	}
	// Collapse the whitespace a substitution run can leave behind.
	trimmed := make([]rune, 0, len(out))
	prevSpace := true
	for _, r := range out {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		trimmed = append(trimmed, r)
	}
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == ' ' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return string(trimmed)
}

// CurrentScreenState is the bounded real probe, for callers (the venue
// companion's privacy mode) that need the same locked/displays-off reading
// the bar's skip rule uses.
func CurrentScreenState() ScreenState { return platformScreenState() }
