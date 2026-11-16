package solarbeam

import (
	"errors"
	"testing"
)

func TestLinkRotationRequiresConfirmedBridgeTeardown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		present   []bool
		probeErr  error
		wantError bool
	}{
		{"already stopped", []bool{false}, nil, false},
		{"stops after termination", []bool{true, false}, nil, false},
		{"snapshot unavailable", []bool{false}, errors.New("snapshot unavailable"), true},
		{"snapshot lost while stopping", []bool{true, false}, errors.New("walk interrupted"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminated, probes := false, 0
			err := stopBridgeForLinkRotationWith("bridge.exe", func(name string) {
				if name != "bridge.exe" {
					t.Fatal("wrong process targeted")
				}
				terminated = true
			}, func(name string) (bool, error) {
				if !terminated || name != "bridge.exe" || probes >= len(tc.present) {
					t.Fatal("unexpected stop verification")
				}
				found := tc.present[probes]
				probes++
				if probes == len(tc.present) {
					return found, tc.probeErr
				}
				return found, nil
			})
			if (err != nil) != tc.wantError || (tc.wantError && !errors.Is(err, errPreviousBridgeStillRunning)) {
				t.Fatalf("unconfirmed teardown did not preserve previous-session authority: %v", err)
			}
			if probes != len(tc.present) {
				t.Fatal("stopped before confirming process absence")
			}
		})
	}
}

func TestLinkRotationRefusesUnknownBridgeName(t *testing.T) {
	if err := stopBridgeForLinkRotationWith("", func(string) {
		t.Fatal("unknown bridge was targeted for termination")
	}, func(string) (bool, error) {
		t.Fatal("unknown bridge was inspected")
		return false, nil
	}); !errors.Is(err, errPreviousBridgeStillRunning) {
		t.Fatal("empty process identity accepted")
	}
}
