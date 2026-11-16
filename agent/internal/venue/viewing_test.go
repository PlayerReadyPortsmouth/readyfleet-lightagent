package venue

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func viewingEnv(t *testing.T, typ proto.MessageType, d proto.ViewingData) proto.Envelope {
	t.Helper()
	env, err := proto.NewEnvelope(typ, uuid.Nil, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestViewingTrackerStartStopAndExpiry(t *testing.T) {
	var tr ViewingTracker
	now := time.Now()
	tr.Handle(viewingEnv(t, proto.MsgViewingStarted, proto.ViewingData{WatcherCount: 2, Kind: "screen"}), now)
	if v := tr.Current(now); !v.Active || v.WatcherCount != 2 || v.Kind != "screen" {
		t.Fatalf("after start: %+v", v)
	}
	tr.Handle(viewingEnv(t, proto.MsgViewingStopped, proto.ViewingData{}), now)
	if tr.Current(now).Active {
		t.Fatal("still active after stop")
	}
	tr.Handle(viewingEnv(t, proto.MsgViewingStarted, proto.ViewingData{WatcherCount: 1, Kind: "screen"}), now)
	if !tr.Current(now.Add(29 * time.Second)).Active {
		t.Fatal("dropped before the renewal window")
	}
	if tr.Current(now.Add(31 * time.Second)).Active {
		t.Fatal("still active 31 s without renewal")
	}
}

// The companion reads this JSON over the status pipe; field names are the contract.
func TestSnapshotViewingContract(t *testing.T) {
	b, _ := json.Marshal(Snapshot{Viewing: Viewing{Active: true, WatcherCount: 1, Kind: "screen"}})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	v, _ := m["viewing"].(map[string]any)
	if v["active"] != true || v["watcherCount"] != float64(1) || v["kind"] != "screen" {
		t.Fatalf("viewing wire form: %s", b)
	}
	for _, banned := range []string{"viewer", "name", "staff"} {
		if _, ok := v[banned]; ok {
			t.Fatalf("viewing must not carry %q", banned)
		}
	}
}
