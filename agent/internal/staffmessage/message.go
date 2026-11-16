// Package staffmessage delivers a staff "Send message" (CmdShowNotification)
// to the screen of a MANAGED Windows venue PC.
//
// The managed agent is a SYSTEM service in session 0: it has no desktop and
// must never spawn UI into a user's session. The venue companion
// (cmd/venuecompanion) already runs as the logged-in CYP user, so the service
// hands the message to it and waits for the companion to say it is on screen:
//
//	C2 ──show_notification──▶ service handler (Run)
//	                              │ Validate: plain text, limits, refusals
//	                              ▼
//	                           Broker ◀── GET  /v1/notify/next  (companion long-poll)
//	                              ▲   ─── POST /v1/notify/ack   (companion: shown / not)
//	                              │
//	                        result: displayed | no_session | not_displayed | refused | invalid
//
// The pipe (`\\.\pipe\ReadyFleet.Venue.Notify.v1`, agent/internal/venue) is a
// third pipe in the existing companion design, with the same DACL and the
// same per-request client check as the mode pipe (exact CYP SID, active
// console session). The status pipe stays read-only and the mode pipe keeps
// exactly its two routes.
//
// Rules that make the result honest:
//
//   - No companion polling ⇒ no_session immediately. The service never
//     reports success for a message nobody collected.
//   - A collected message is "displayed" only on the companion's ack, which
//     it sends after its banner window is actually visible (a tray balloon
//     is sent too, but never counts on its own). A locked screen is acked
//     as not shown and reported as no_session.
//   - Timeouts are failures: not collected within ClaimTimeout ⇒ no_session;
//     collected but not acked within AckTimeout ⇒ not_displayed.
//
// Children's sessions: kind "connection_request" (a consent question, BYOD
// only) and ANY URL are refused outright on managed PCs — a message there is
// a statement with nothing to click. Text is plain: control and Unicode
// format characters (bidi overrides, zero-width joiners) are stripped and the
// limits in proto are enforced on what remains, by refusal, not truncation.
//
// Trust limit, stated plainly: the ack is the companion's attestation from
// the CYP user's session. Code running as that user could collect or ack a
// message itself; "displayed" means the signed companion path reported it on
// screen, not that a person read it.
package staffmessage

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// ConnectionRequestKind is the BYOD consent question; refused on managed PCs.
const ConnectionRequestKind = "connection_request"

// maxRawBytes bounds each field before sanitising, so a hostile payload is
// rejected without walking megabytes of text.
const maxRawBytes = 4096

const maxKindBytes = 64

// Severity values (proto.ShowNotificationArgs.Severity). Unknown ⇒ info.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Message is a validated staff message: safe to render as plain text.
type Message struct {
	ID          string `json:"id,omitempty"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Severity    string `json:"severity"`
	ApprovalURL string `json:"approval_url,omitempty"`
	RequesterID string `json:"requester_id,omitempty"`
}

// Failure is a refusal or validation error carrying its report code.
type Failure struct {
	Code   string
	Detail string
}

func (f *Failure) Error() string { return f.Code + ": " + f.Detail }

func fail(code, detail string) *Failure { return &Failure{Code: code, Detail: detail} }

// Validate turns wire args into a Message for a managed venue PC. Refusals
// are checked first, so a connection request or link is always reported as
// refused even when its text is also invalid.
func Validate(args proto.ShowNotificationArgs) (Message, *Failure) {
	if args.Kind == ConnectionRequestKind {
		return Message{}, fail(proto.NotifyRefused, "connection requests are not supported on managed venue PCs")
	}
	if strings.TrimSpace(args.URL) != "" {
		return Message{}, fail(proto.NotifyRefused, "links are not allowed in messages to managed venue PCs")
	}
	return validateText(args.Kind, args.Title, args.Body, args.Severity)
}

// Revalidate is the companion's defence-in-depth check on a message received
// from the service pipe: the same rules, applied again before rendering.
func Revalidate(m Message) (Message, *Failure) {
	if m.Kind == ConnectionRequestKind && !ValidApprovalURL(m.ApprovalURL) {
		return Message{}, fail(proto.NotifyRefused, "invalid connection approval link")
	}
	out, f := validateText(m.Kind, m.Title, m.Body, m.Severity)
	if f != nil {
		return Message{}, f
	}
	if !ValidID(m.ID) {
		return Message{}, fail(proto.NotifyInvalid, "message id is malformed")
	}
	out.ID = m.ID
	out.ApprovalURL = m.ApprovalURL
	out.RequesterID = m.RequesterID
	return out, nil
}

func validateText(kind, title, body, severity string) (Message, *Failure) {
	kind = strings.TrimSpace(kind)
	if kind == "" || len(kind) > maxKindBytes || !utf8.ValidString(kind) || strings.IndexFunc(kind, badKindRune) >= 0 {
		return Message{}, fail(proto.NotifyInvalid, "kind is required (up to 64 letters, digits, '_', '-' or '.')")
	}
	if len(title) > maxRawBytes || len(body) > maxRawBytes {
		return Message{}, fail(proto.NotifyInvalid, "title or body is too long")
	}
	t := SanitizeTitle(title)
	b := SanitizeBody(body)
	switch {
	case t == "":
		return Message{}, fail(proto.NotifyInvalid, "title is required")
	case b == "":
		return Message{}, fail(proto.NotifyInvalid, "body is required")
	case utf8.RuneCountInString(t) > proto.StaffMessageMaxTitleRunes:
		return Message{}, fail(proto.NotifyInvalid, "title is longer than 80 characters")
	case utf8.RuneCountInString(b) > proto.StaffMessageMaxBodyRunes:
		return Message{}, fail(proto.NotifyInvalid, "body is longer than 300 characters")
	}
	return Message{Kind: kind, Title: t, Body: b, Severity: normalSeverity(severity)}, nil
}

func badKindRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
}

func normalSeverity(s string) string {
	switch s {
	case SeverityWarning, SeverityError:
		return s
	default:
		return SeverityInfo
	}
}

// dropRune reports characters removed from display text: C0/C1 controls,
// Unicode format characters (bidi overrides, zero-width), line/paragraph
// separators, private-use, and U+FFFD from invalid UTF-8.
func dropRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) ||
		unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Co, r) || r == utf8.RuneError
}

// SanitizeTitle returns one line: controls stripped, whitespace collapsed.
func SanitizeTitle(s string) string {
	return strings.Join(strings.FieldsFunc(strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if dropRune(r) {
			return -1
		}
		return r
	}, s), unicode.IsSpace), " ")
}

// SanitizeBody keeps line breaks (at most one blank line in a row) and
// otherwise applies SanitizeTitle's rules per line.
func SanitizeBody(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = SanitizeTitle(line)
		if line == "" {
			blank++
			if blank > 1 || len(out) == 0 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// ValidID reports a broker-issued message id: 32 lowercase hex characters.
func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Companion ack reasons for a message it could not show.
const (
	ReasonLocked         = "locked"
	ReasonDisplayFailed  = "display_failed"
	ReasonInvalid        = "invalid"
	ReasonSessionChanged = "session_changed"
)

// Ack is the companion's answer for one collected message.
type Ack struct {
	ID        string `json:"id"`
	Displayed bool   `json:"displayed"`
	// Via is "banner" or "banner+tray" when Displayed. A tray balloon alone
	// never counts: Shell_NotifyIcon succeeds even when Focus Assist hides
	// the balloon, so it is not evidence of display.
	Via string `json:"via,omitempty"`
	// Reason is one of the Reason* constants when not Displayed.
	Reason string `json:"reason,omitempty"`
}

// Validate checks an ack's shape (the broker separately checks the id is one
// it issued and that the message was collected).
func (a Ack) Validate() error {
	if !ValidID(a.ID) {
		return errors.New("ack id is malformed")
	}
	if a.Displayed {
		switch a.Via {
		case "banner", "banner+tray":
		default:
			return errors.New("ack via is not recognised")
		}
		if a.Reason != "" {
			return errors.New("a displayed ack carries no reason")
		}
		return nil
	}
	if a.Via != "" {
		return errors.New("an undisplayed ack carries no via")
	}
	switch a.Reason {
	case ReasonLocked, ReasonDisplayFailed, ReasonInvalid, ReasonSessionChanged:
		return nil
	}
	return errors.New("ack reason is not recognised")
}

// Only the authenticated ReadyApp approval site may be opened by the companion.
func ValidApprovalURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "readyapp.player-ready.co.uk" && u.User == nil && u.Fragment == ""
}
func ConnectionMessage(args proto.ShowNotificationArgs) (Message, *Failure) {
	if args.Kind != ConnectionRequestKind || !ValidApprovalURL(args.URL) {
		return Message{}, fail(proto.NotifyRefused, "invalid connection approval link")
	}
	m, f := validateText(args.Kind, args.Title, args.Body, args.Severity)
	m.ApprovalURL = args.URL
	m.RequesterID = args.RequesterID
	return m, f
}
