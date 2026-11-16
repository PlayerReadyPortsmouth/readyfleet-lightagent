//go:build !linux && !windows

package sessionindicator

// readScreenState knows nothing on a platform that has no indicator.
//
// Every field Unknown, which Decide turns into SHOW. That is consistent
// rather than merely convenient: New() on this platform already returns
// ErrUnsupported, so no Show ever reaches the decision, and if one
// somehow did, "I could not find out" must not read as "nobody is
// there".
func readScreenState() ScreenState { return ScreenState{} }
