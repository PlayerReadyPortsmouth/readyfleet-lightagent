package sacposture

import (
	"errors"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

type fakeReader struct {
	state   uint64
	present bool
	err     error
	build   int
}

func (f fakeReader) PolicyState() (uint64, bool, error) { return f.state, f.present, f.err }
func (f fakeReader) OSBuild() int                       { return f.build }

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		r    Reader
		want string
	}{
		{"off", fakeReader{state: 0, present: true, build: 26100}, proto.SACStateOff},
		{"on", fakeReader{state: 1, present: true, build: 26100}, proto.SACStateOn},
		{"evaluation", fakeReader{state: 2, present: true, build: 22631}, proto.SACStateEvaluation},
		{"unrecognised value", fakeReader{state: 7, present: true, build: 26100}, proto.SACStateUnknown},
		{"read error", fakeReader{err: errors.New("denied"), build: 26100}, proto.SACStateUnknown},
		{"missing on win10", fakeReader{build: 19045}, proto.SACStateOff},
		{"missing on win11", fakeReader{build: 22621}, proto.SACStateUnknown},
		{"missing, build unreadable", fakeReader{}, proto.SACStateUnknown},
		{"no reader (non-Windows)", nil, proto.SACStateUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.r); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
