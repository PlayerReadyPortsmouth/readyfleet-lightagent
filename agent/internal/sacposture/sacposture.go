// Package sacposture reads the Windows Smart App Control (SAC) state so the
// controller can see which machines would refuse an unsigned-reputation agent
// build before an update target moves them. On 26 Sep 2026 agent 0.49.0 looped
// and bricked SAC-on laptops; the controller had no way to know which ones
// were SAC-on. This package is read-only: it changes no behaviour on the box.
//
// Source: HKLM\SYSTEM\CurrentControlSet\Control\CI\Policy, DWORD
// VerifiedAndReputablePolicyState (0 off, 1 on/enforce, 2 evaluation). SAC
// only exists on Windows 11 22H2 (build 22621) and later; on an older build a
// missing value means SAC cannot be on, so it reads as off. Anything we cannot
// read with certainty is "unknown" — never guessed as off.
package sacposture

import (
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// MinSACBuild is the first Windows build that ships Smart App Control.
const MinSACBuild = 22621

// Reader is the registry seam; tests fake it.
type Reader interface {
	// PolicyState returns VerifiedAndReputablePolicyState; present=false
	// when the value (or key) does not exist, err for any other failure.
	PolicyState() (state uint64, present bool, err error)
	// OSBuild returns CurrentBuild (0 when unreadable).
	OSBuild() int
}

// Classify maps a reading to one of the proto.SACState* values.
func Classify(r Reader) string {
	if r == nil {
		return proto.SACStateUnknown
	}
	v, present, err := r.PolicyState()
	if err != nil {
		return proto.SACStateUnknown
	}
	if !present {
		if b := r.OSBuild(); b > 0 && b < MinSACBuild {
			return proto.SACStateOff
		}
		return proto.SACStateUnknown
	}
	switch v {
	case 0:
		return proto.SACStateOff
	case 1:
		return proto.SACStateOn
	case 2:
		return proto.SACStateEvaluation
	}
	return proto.SACStateUnknown
}

// Probe reads the live state; it is the transport's hello hook.
func Probe() (string, time.Time) {
	return Classify(systemReader()), time.Now().UTC()
}
