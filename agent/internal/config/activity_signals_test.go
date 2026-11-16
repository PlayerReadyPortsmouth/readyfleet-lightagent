package config

import (
	"encoding/json"
	"testing"
)

// activity_signals is a kill switch: only an explicit false stops the helper;
// usage_stats is read as its alias for one release, and loses to it.
func TestActivitySignalsKillSwitch(t *testing.T) {
	for in, want := range map[string]bool{
		`{}`: true, `{"activity_signals":false}`: false, `{"activity_signals":true}`: true,
		`{"usage_stats":false}`: false, `{"usage_stats":true}`: true,
		`{"activity_signals":true,"usage_stats":false}`: true,
	} {
		var c Config
		if err := json.Unmarshal([]byte(in), &c); err != nil {
			t.Fatal(err)
		}
		if c.ActivitySignalsEnabled() != want {
			t.Errorf("%s: got %v want %v", in, c.ActivitySignalsEnabled(), want)
		}
	}
}
