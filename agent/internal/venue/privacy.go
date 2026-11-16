package venue

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// PrivacyMaxAge is the hard backstop: privacy mode never outlives this, even
// if every other signal is lost.
const PrivacyMaxAge = 12 * time.Hour

// PrivacyView is what the service tells the companion. Active is the service's
// permission; the companion still applies its own fail-safe rules (fresh
// snapshot, a viewer present, session match, not locked).
type PrivacyView struct {
	Active    bool   `json:"active"`
	SessionID string `json:"sessionId,omitempty"`
	Label     string `json:"label,omitempty"` // "First I.", server-built
	Input     bool   `json:"input,omitempty"` // local keyboard/mouse lock requested
}

// Privacy outcomes the companion reports.
const (
	PrivacyOn      = "on"
	PrivacyPartial = "partial"
	PrivacyFailed  = "failed"
	// PrivacyReleased: the local break-glass chord ended privacy on this PC.
	PrivacyReleased = "released"
)

// PrivacyOutcome is the companion's report, validated against the live state.
type PrivacyOutcome struct {
	SessionID string `json:"sessionId"`
	Outcome   string `json:"outcome"`
	Monitors  int    `json:"monitors,omitempty"`
	Covered   int    `json:"covered,omitempty"`
}

// PrivacyState is the service-side record of the one session that asked for
// privacy. Memory only: an agent restart forgets it, which is the safe
// direction (the screen returns to normal).
type PrivacyState struct {
	mu       sync.Mutex
	view     PrivacyView
	since    time.Time
	released bool
}

// Begin records a session's request, called when start_solarbeam is accepted.
func (p *PrivacyState) Begin(sessionID, label string, input bool, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(label) > 64 {
		label = label[:64]
	}
	p.view = PrivacyView{Active: sessionID != "", SessionID: sessionID, Label: label, Input: input}
	p.since, p.released = now, false
}

// End clears privacy for sessionID (empty = whatever is active). Reports whether
// something was cleared.
func (p *PrivacyState) End(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view.SessionID == "" || (sessionID != "" && sessionID != p.view.SessionID) {
		return false
	}
	p.view = PrivacyView{}
	return true
}

// Current is the view to publish at now (expired state publishes inactive).
func (p *PrivacyState) Current(now time.Time) PrivacyView {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view.Active && now.Sub(p.since) > PrivacyMaxAge {
		p.view = PrivacyView{}
	}
	return p.view
}

// StartGrace is how long a requested session may take to appear as the engine's
// current session before the request is considered dead.
const StartGrace = 2 * time.Minute

// Reconcile drops privacy that no longer belongs to the live session: the
// engine's current session is a different one, or there is none once the start
// grace has passed (session ended, expired or failed to start).
func (p *PrivacyState) Reconcile(currentSessionID string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.view.Active {
		return
	}
	if (currentSessionID != "" && currentSessionID != p.view.SessionID) ||
		(currentSessionID == "" && now.Sub(p.since) > StartGrace) {
		p.view = PrivacyView{}
	}
}

// Matches reports whether sessionID is the session privacy is active for.
func (p *PrivacyState) Matches(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.view.Active && p.view.SessionID == sessionID
}

func handlePrivacyOutcome(store *Store, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	b, err := io.ReadAll(r.Body)
	var o PrivacyOutcome
	if err != nil || json.Unmarshal(b, &o) != nil || o.SessionID == "" || len(o.SessionID) > 128 ||
		(o.Outcome != PrivacyOn && o.Outcome != PrivacyPartial && o.Outcome != PrivacyFailed && o.Outcome != PrivacyReleased) ||
		o.Monitors < 0 || o.Monitors > 32 || o.Covered < 0 || o.Covered > o.Monitors {
		http.Error(w, "bad privacy outcome", http.StatusBadRequest)
		return
	}
	store.OnPrivacyOutcome(o)
	w.WriteHeader(http.StatusNoContent)
}
