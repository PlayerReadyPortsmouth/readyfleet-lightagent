package transport

import (
	"testing"
	"time"
)

func TestBackoff_Schedule(t *testing.T) {
	b := NewBackoff(time.Second, 60*time.Second)

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second,
		60 * time.Second,
	}
	for i, w := range want {
		got := b.Next()
		if got != w {
			t.Errorf("step %d: got %v want %v", i, got, w)
		}
	}

	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Errorf("after reset: got %v want 1s", got)
	}
}

func TestBackoff_Defaults(t *testing.T) {
	b := NewBackoff(0, 0)
	if b.Min != time.Second || b.Max != 60*time.Second {
		t.Errorf("defaults: min=%v max=%v", b.Min, b.Max)
	}
}
