// Package venue exposes a deliberately read-only view of the managed agent to
// its user-session companion. The companion never reads enrollment credentials
// or receives an arbitrary-command/elevation interface.
package venue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const ProtocolVersion = 1
const MaxResponseBytes = 64 * 1024
const FreshFor = 15 * time.Second

type Component struct {
	Installed   string `json:"installed,omitempty"`
	Target      string `json:"target,omitempty"`
	UpdateState string `json:"updateState"`
}

type Service struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	StartType string `json:"startType"`
}

// RemotePresence is an observation of actual SolarBeam room membership.
// Keys identify connections only for notification deduplication, never display.
type RemoteViewer struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type RemotePresence struct {
	Known   bool           `json:"known"`
	Count   int            `json:"count"`
	Viewers []RemoteViewer `json:"viewers,omitempty"`
}

// ActivitySignals says which staff-device signals are running, so the tray
// can show it (transparency). Read-only like everything here.
type ActivitySignals struct {
	Foreground bool `json:"foreground"` // app usage (foreground program names)
	DNS        bool `json:"dns"`        // listed-website counts
}

type Snapshot struct {
	HostingKnown       bool            `json:"hostingKnown"`
	HostingEnabled     bool            `json:"hostingEnabled"`
	HostingError       string          `json:"hostingError,omitempty"`
	CurrentSessionID   string          `json:"currentSessionId,omitempty"`
	HostingRevision    string          `json:"hostingRevision,omitempty"`
	Remote             RemotePresence  `json:"remote"`
	Privacy            PrivacyView     `json:"privacy"`
	Viewing            Viewing         `json:"viewing"`
	ViewingBar         bool            `json:"viewingBar"` // config.json viewing_bar; absent = false
	ActivitySignals    ActivitySignals `json:"activity_signals"`
	Protocol           int             `json:"protocol"`
	ObservedAt         time.Time       `json:"observedAt"`
	MachineID          string          `json:"machineId"`
	Connected          bool            `json:"connected"`
	ConnectedSince     time.Time       `json:"connectedSince"`
	Agent              Component       `json:"agent"`
	SolarBeam          Component       `json:"solarbeam"`
	Services           []Service       `json:"services"`
	ModeState          string          `json:"modeState"`
	EnforcementEnabled bool            `json:"enforcementEnabled"`
	Limitations        []string        `json:"limitations"`
}

// Store is fed by the service, not by requests from an untrusted desktop. Slow
// Windows queries cannot turn a tray refresh into unbounded privileged work.
type Store struct {
	mu    sync.RWMutex
	value Snapshot
	// OnPrivacyOutcome receives the companion's report of what privacy mode
	// achieved. nil = the route answers 404.
	OnPrivacyOutcome func(PrivacyOutcome)
}

func (s *Store) Set(v Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = clone(v)
}
func (s *Store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.value)
}
func clone(v Snapshot) Snapshot {
	v.Services = append([]Service(nil), v.Services...)
	v.Remote.Viewers = append([]RemoteViewer(nil), v.Remote.Viewers...)
	v.Limitations = append([]string(nil), v.Limitations...)
	return v
}

func (v Snapshot) Validate(now time.Time) error {
	if v.Protocol != ProtocolVersion {
		return fmt.Errorf("unsupported companion protocol")
	}
	if v.ObservedAt.IsZero() || now.Sub(v.ObservedAt) > FreshFor || v.ObservedAt.After(now.Add(5*time.Second)) {
		return fmt.Errorf("agent status is stale; refresh required")
	}
	return nil
}

// Handler has no mutation routes except the narrow privacy-outcome report
// (reporting only; it cannot start, stop or change anything). Even a forged request cannot
// stop services, check/download updates, change modes or grant a desktop here.
func Handler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/privacy-outcome" && store.OnPrivacyOutcome != nil {
			handlePrivacyOutcome(store, w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "read-only companion endpoint", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/status" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.Get())
	})
}

func Unknown() Snapshot {
	return Snapshot{Protocol: ProtocolVersion, Agent: Component{UpdateState: "unknown"}, SolarBeam: Component{UpdateState: "unknown"}, ModeState: "not configured", Limitations: []string{
		"Read-only preview: no startup, service, account or mode changes.",
		"Release targets and manual update checks are not connected yet.",
		"Login enforcement and staff PIN authorisation are not enabled.",
	}}
}
