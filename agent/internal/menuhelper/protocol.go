// Package menuhelper is the light agent's side of the macOS menu-bar helper.
//
// The agent is deliberately CGO-free and cross-compiled, so it cannot own an
// NSStatusItem itself. Instead a tiny AppKit app, "ReadyFleet Menu.app"
// (source: agent/darwin/menu), draws the menu-bar item and the agent talks to
// it over a loopback Unix socket in the agent's own data directory
// (~/Library/Application Support/ReadyFleet/menu.sock, mode 0600). The agent
// is the listener and launches the helper (Launcher); the helper connects,
// reconnects if the agent restarts, and exits by itself once the agent is gone.
//
// Wire format: one JSON object per line, UTF-8, no embedded newlines. Every
// object carries "type". The agent sends status, prompt and cancel; the helper
// sends hello and decision. Unknown fields are ignored on both sides so either
// end can be upgraded first; an unknown type is logged and dropped.
//
// The helper is a display, not a decision maker. A connection request is
// still decided by agent/internal/consent: Server implements consent.Asker by
// showing the question in the menu bar and reporting what the person pressed,
// and every path where no answer arrives (helper not running, disconnected
// mid-question, timed out, cancelled by the C2) is a decline, with the reason
// in the log. The helper never sees the request URL or the requester's id;
// opening the approval page stays with the agent.
package menuhelper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Message types on the wire.
const (
	TypeStatus   = "status"   // agent → helper
	TypePrompt   = "prompt"   // agent → helper
	TypeCancel   = "cancel"   // agent → helper
	TypeHello    = "hello"    // helper → agent
	TypeDecision = "decision" // helper → agent
)

// Status is the agent's current state, sent whenever it changes and again to
// every helper that connects. The helper shows exactly these fields; it
// infers nothing. Venue and DeviceName are whatever the agent knows — today
// the venue id and, until the C2 hands the agent its display name, an empty
// device name, which the helper renders as a neutral placeholder.
type Status struct {
	Type         string `json:"type"`
	Online       bool   `json:"online"`
	Venue        string `json:"venue"`
	DeviceName   string `json:"device_name"`
	SessionLive  bool   `json:"session_live"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Prompt asks the person at the Mac to approve or decline a connection
// request. ID is the agent's own correlation id for this question (not the
// ReadyApp request id, which may be absent). Requester is the headline
// ReadyApp wrote ("Jo Bloggs wants to connect to your device"); Detail is
// the notification body. ExpiresAt is when the agent stops waiting and
// counts the silence as a decline; the helper closes the question then too.
type Prompt struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Requester string    `json:"requester"`
	Detail    string    `json:"detail,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Cancel withdraws a prompt the helper may still be showing: the owner
// decided it in ReadyApp, the agent timed out, or the agent is shutting down.
type Cancel struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Hello is the helper's first line on every connection. The agent answers
// with its current Status so a freshly (re)started helper is never blank.
type Hello struct {
	Type    string `json:"type"`
	Version string `json:"version,omitempty"`
}

// Decision is the person's answer to a Prompt. Approve is the only thing the
// helper gets to say; anything else it does (closing the window, quitting)
// is silence, which the agent reads as a decline.
type Decision struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Approve bool   `json:"approve"`
}

// Encode renders one message as a JSON line, stamping its type. It accepts
// only the five wire types so a typo in a caller is a compile-time or
// immediate error rather than a line the helper silently drops.
func Encode(msg any) ([]byte, error) {
	var v any
	switch m := msg.(type) {
	case Status:
		m.Type = TypeStatus
		v = m
	case Prompt:
		m.Type = TypePrompt
		v = m
	case Cancel:
		m.Type = TypeCancel
		v = m
	case Hello:
		m.Type = TypeHello
		v = m
	case Decision:
		m.Type = TypeDecision
		v = m
	default:
		return nil, fmt.Errorf("menuhelper: cannot encode %T", msg)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if bytes.ContainsAny(b, "\n\r") {
		// encoding/json escapes control characters inside strings, so this
		// cannot happen; the check documents the line discipline anyway.
		return nil, fmt.Errorf("menuhelper: encoded message contains a line break")
	}
	return append(b, '\n'), nil
}

// ErrUnknownType reports a line whose "type" this build does not know. The
// reader logs and skips it rather than dropping the connection, so a newer
// helper can talk to an older agent.
type ErrUnknownType struct{ Type string }

func (e *ErrUnknownType) Error() string {
	return "menuhelper: unknown message type " + fmt.Sprintf("%q", e.Type)
}

// Decode parses one line into its typed message (Status, Prompt, Cancel,
// Hello or Decision, by value). Unknown fields are ignored.
func Decode(line []byte) (any, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, fmt.Errorf("menuhelper: empty line")
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return nil, fmt.Errorf("menuhelper: malformed line: %w", err)
	}
	var (
		v   any
		err error
	)
	switch head.Type {
	case TypeStatus:
		var m Status
		err = json.Unmarshal(line, &m)
		v = m
	case TypePrompt:
		var m Prompt
		err = json.Unmarshal(line, &m)
		if err == nil && m.ID == "" {
			err = fmt.Errorf("menuhelper: prompt without an id")
		}
		v = m
	case TypeCancel:
		var m Cancel
		err = json.Unmarshal(line, &m)
		v = m
	case TypeHello:
		var m Hello
		err = json.Unmarshal(line, &m)
		v = m
	case TypeDecision:
		var m Decision
		err = json.Unmarshal(line, &m)
		if err == nil && m.ID == "" {
			err = fmt.Errorf("menuhelper: decision without an id")
		}
		v = m
	default:
		return nil, &ErrUnknownType{Type: head.Type}
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}
