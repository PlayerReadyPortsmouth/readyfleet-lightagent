package venue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyBoundary(t *testing.T) {
	v := Unknown()
	v.ObservedAt = time.Now().UTC()
	v.Agent.Installed = "test-agent"
	v.SolarBeam.Installed = "test-engine"
	store := &Store{}
	store.Set(v)
	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v1/status", 200}, {"POST", "/v1/status", 405}, {"POST", "/v1/mode", 405},
		{"GET", "/v1/status?command=stop", 404}, {"GET", "/v1/update", 404}, {"DELETE", "/v1/status", 405},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"script":"stop"}`))
			w := httptest.NewRecorder()
			Handler(store).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want=%d", w.Code, tt.want)
			}
			if tt.want == 200 {
				var got Snapshot
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Agent.Installed != "test-agent" || got.SolarBeam.Installed != "test-engine" {
					t.Fatal("component status lost")
				}
				if got.EnforcementEnabled {
					t.Fatal("preview must never claim enforcement")
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("status cached")
				}
			}
		})
	}
}

func TestFreshness(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		at      time.Time
		version int
		ok      bool
	}{
		{"fresh", now, 1, true}, {"old", now.Add(-16 * time.Second), 1, false}, {"future", now.Add(6 * time.Second), 1, false}, {"missing", time.Time{}, 1, false}, {"new protocol", now, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (Snapshot{ObservedAt: tt.at, Protocol: tt.version}).Validate(now)
			if (err == nil) != tt.ok {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}

func TestStoreCopiesSlices(t *testing.T) {
	s := &Store{}
	v := Unknown()
	v.Services = []Service{{Name: "approved", State: "stopped"}}
	s.Set(v)
	v.Services[0].State = "running"
	got := s.Get()
	got.Limitations[0] = "changed"
	if s.Get().Services[0].State != "stopped" || s.Get().Limitations[0] == "changed" {
		t.Fatal("status aliases caller memory")
	}
}

func TestModePreviewNeverAuthorises(t *testing.T) {
	v := Unknown()
	v.Connected = true
	v.Services = []Service{{Name: "ReviewedVRService", State: "stopped"}}
	for _, mode := range []Mode{APClubs, VR} {
		p, err := Plan(mode, v)
		if err != nil {
			t.Fatal(err)
		}
		if p.CanApply || len(p.Blockers) == 0 {
			t.Fatal("read-only observations authorised a transition")
		}
		joined := strings.Join(p.Steps, " ")
		if mode == APClubs && !strings.Contains(joined, "Keep VR services stopped") {
			t.Fatal("AP must retain stopped VR")
		}
		if mode == VR && !strings.Contains(joined, "Start and verify") {
			t.Fatal("VR verification missing")
		}
	}
	if _, err := Plan(Mode("shell"), v); err == nil {
		t.Fatal("arbitrary operation accepted")
	}
}

func TestUnknownServicesDoNotPassPreflight(t *testing.T) {
	p, err := Plan(VR, Snapshot{Services: []Service{{Name: "VRService", State: "unknown"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Blockers, " "), "stopped state not verified") {
		t.Fatal("unknown observation treated as stopped")
	}
}

func TestHTTPFlow(t *testing.T) {
	s := &Store{}
	v := Unknown()
	v.ObservedAt = time.Now()
	s.Set(v)
	srv := httptest.NewServer(Handler(s))
	defer srv.Close()
	r, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var got Snapshot
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
}
