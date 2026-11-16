// Package signedin reports which account is signed in to the machine's
// interactive session, for the Fleet "who is on it" column.
//
// Privacy: the account name and logon time of the current session only (no
// history, passwords or tokens), plus the console session's lock flag
// (ConsoleLocked) as the attended-state fallback. This package reads no
// process or window information. The wider activity-signal contract (attended
// state on every venue PC; foreground program and listed-domain counts on
// staff devices only) is in docs/security.md, "Activity signals on venue PCs".
package signedin

import (
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Reader reads the interactive session's account. It returns an empty user
// (and zero time) when nobody is signed in, e.g. at the sign-in screen.
type Reader func() (user string, since time.Time)

// Report turns a Reader result into the wire value. Never nil: an empty User
// is the explicit "nobody is signed in", distinct from an older agent that
// sends nothing.
func Report(read Reader) *proto.SignedInReport {
	if read == nil {
		return nil
	}
	user, since := read()
	r := &proto.SignedInReport{User: user}
	if user != "" && !since.IsZero() {
		s := since.UTC()
		r.Since = &s
	}
	return r
}
