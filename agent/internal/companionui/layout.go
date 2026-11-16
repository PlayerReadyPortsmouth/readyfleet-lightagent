package companionui

import "strings"

// The panel's controls, and where each one sits on each page, in the 420-DIP
// design. This file is platform-neutral so the whole layout is checked by
// tests; window_windows.go only creates, shows and moves what it says.
//
// Tightened on 15 Sep 2026 after the PC-1 photo. The panel had a tall empty
// gap under the two status lines and a stray "unknown" update line. Three
// disabled full-width buttons dominated it. The sign-in row, Refresh and the
// footer were cramped against the bottom. Every control and what it does is
// unchanged. Only sizes, positions and a small "Staff actions" caption differ.

type fontKind uint8

const (
	fontNormal fontKind = iota // 14
	fontSmall                  // 12
	fontBold                   // 14 semibold
	fontTitle                  // 19 bold
)

// panelControl is one child window. Style holds extra window styles; BUTTON
// controls are owner-drawn in addition (window_windows.go).
type panelControl struct {
	ID          int
	Class, Text string
	Font        fontKind
	Style       uint32
}

const (
	wsTabStop = 0x10000
	wsVScroll = 0x200000
	wsBorder  = 0x800000
	lbsNotify = 0x1
	// LBS_NOINTEGRALHEIGHT. Without it the session list snaps its own height
	// to whole rows. On the VM (15 Sep 2026) that left the panel repainting
	// itself continuously once the "Session I'm viewing" tab opened: one CPU
	// core busy, WM_PAINT/WM_ERASEBKGND/WM_CTLCOLORLISTBOX thousands of times a
	// second, and controls that never finished drawing.
	lbsNoIntegralHeight = 0x100
	esMultiRO           = 0x4 | 0x800 // ES_MULTILINE | ES_READONLY
	esAutoVert          = 0x40
)

var panelControls = []panelControl{
	{1, "STATIC", "ReadyFleet", fontTitle, 0}, {2, "BUTTON", "Close", fontSmall, wsTabStop},
	{3, "STATIC", "Reading device status…", fontSmall, 0},
	{10, "BUTTON", "This PC", fontBold, wsTabStop}, {11, "BUTTON", "Session I’m viewing", fontBold, wsTabStop},
	{45, "LISTBOX", "", fontNormal, wsTabStop | wsVScroll | wsBorder | lbsNotify | lbsNoIntegralHeight},
	{20, "STATIC", "", fontNormal, 0}, {21, "STATIC", "", fontSmall, 0},
	{25, "STATIC", "Staff actions", fontSmall, 0},
	{30, "BUTTON", "SolarBeam availability", fontNormal, wsTabStop}, {31, "BUTTON", "Disconnect incoming session", fontNormal, wsTabStop},
	{32, "BUTTON", "Check update status", fontNormal, wsTabStop}, {33, "BUTTON", "Open ReadyApp in browser", fontNormal, wsTabStop},
	{40, "BUTTON", "Review waiting person", fontNormal, wsTabStop}, {41, "BUTTON", "Take exclusive control", fontNormal, wsTabStop},
	{42, "BUTTON", "Release · restore shared input", fontNormal, wsTabStop}, {43, "BUTTON", "Focus browser", fontNormal, wsTabStop}, {44, "BUTTON", "Leave my connection", fontNormal, wsTabStop},
	{24, "STATIC", "Browser content is visible to connected viewers.", fontSmall, 0},
	{34, "BUTTON", "Sign in", fontNormal, wsTabStop}, {35, "BUTTON", "Details", fontNormal, wsTabStop}, {36, "BUTTON", "Shortcuts", fontNormal, wsTabStop},
	{22, "STATIC", "", fontSmall, 0}, {23, "STATIC", "Ctrl+Alt+R · panel", fontSmall, 0}, {37, "BUTTON", "Refresh", fontSmall, wsTabStop},
	{60, "STATIC", "", fontTitle, 0}, {61, "EDIT", "", fontNormal, wsTabStop | wsVScroll | esMultiRO | esAutoVert},
	{62, "BUTTON", "Confirm", fontBold, wsTabStop}, {63, "BUTTON", "Back", fontNormal, wsTabStop},
	{64, "EDIT", "", fontSmall, wsTabStop | wsVScroll | esMultiRO | esAutoVert},
	{74, "STATIC", "Open panel", fontNormal, 0}, {70, "EDIT", "", fontNormal, wsTabStop | wsBorder},
	{75, "STATIC", "Review admission", fontNormal, 0}, {71, "EDIT", "", fontNormal, wsTabStop | wsBorder},
	{76, "STATIC", "Review control", fontNormal, 0}, {72, "EDIT", "", fontNormal, wsTabStop | wsBorder},
	{77, "STATIC", "Restore shared input", fontNormal, 0}, {73, "EDIT", "", fontNormal, wsTabStop | wsBorder},
	{78, "STATIC", "Use Ctrl+Alt plus a letter; add Shift if needed. Leave a field blank to disable it. Conflicts never replace another application’s shortcut.", fontSmall, 0},
	{79, "BUTTON", "Save shortcuts", fontBold, wsTabStop},
}

// PanelContentHeight is the design height every page fits in.
const PanelContentHeight = 548

// panelState is what decides which controls show.
type panelState struct {
	Page, Tab     int // page: 0 main, 1 review, 2 details, 3 shortcuts
	HostingActive bool
}

// controlRect is where a control sits for a state, and whether it shows.
func controlRect(id int, st panelState) (Rect, bool) {
	r := func(x, y, w, h int32) (Rect, bool) { return Rect{x, y, x + w, y + h}, true }
	// Header, tabs and footer are on every page.
	switch id {
	case 1:
		return r(20, 14, 290, 30)
	case 2:
		return r(334, 16, 66, 28)
	case 3:
		return r(20, 44, 380, 20)
	case 10:
		return r(20, 74, 186, 34)
	case 11:
		return r(214, 74, 186, 34)
	case 22:
		return r(20, 484, 380, 34)
	case 23:
		return r(20, 524, 280, 20)
	case 37:
		return r(310, 518, 90, 28)
	}
	switch st.Page {
	case 0:
		switch {
		case id >= 34 && id <= 36:
			return r(20+int32(id-34)*130, 438, 120, 34)
		case st.Tab == 0:
			switch id {
			case 20:
				return r(20, 122, 380, 42)
			case 21:
				return r(20, 166, 380, 36)
			case 25:
				return r(20, 212, 380, 18)
			case 30, 31, 32, 33:
				return r(20, 234+int32(id-30)*40, 380, 34)
			case 24:
				if st.HostingActive {
					return r(20, 396, 380, 20)
				}
			}
		case st.Tab == 1:
			switch id {
			case 45:
				return r(20, 122, 380, 52)
			case 20:
				return r(20, 178, 380, 38)
			case 21:
				return r(20, 218, 380, 32)
			case 40, 41, 42, 43, 44:
				return r(20, 256+int32(id-40)*35, 380, 30)
			}
		}
	case 1:
		switch id {
		case 60:
			return r(20, 122, 380, 30)
		case 61:
			return r(20, 158, 380, 222)
		case 62:
			return r(20, 390, 380, 38)
		case 63:
			return r(20, 436, 380, 34)
		}
	case 2:
		switch id {
		case 64:
			return r(20, 122, 380, 306)
		case 63:
			return r(20, 436, 380, 34)
		}
	case 3:
		switch {
		case id >= 74 && id <= 77:
			return r(20, 132+int32(id-74)*46, 180, 24)
		case id >= 70 && id <= 73:
			return r(210, 128+int32(id-70)*46, 190, 30)
		case id == 78:
			return r(20, 312, 380, 64)
		case id == 79:
			return r(20, 390, 380, 38)
		case id == 63:
			return r(20, 436, 380, 34)
		}
	}
	return Rect{}, false
}

// VersionText is the installed-versions line under the status. An update
// state is shown only when it says something: an empty or "unknown" state
// (the managed service reports "unknown" until update checks exist) is
// dropped rather than printed raw.
func VersionText(d Device) string {
	var parts []string
	if d.AgentVersion != "" {
		parts = append(parts, "Fleet "+d.AgentVersion)
	}
	if d.EngineVersion != "" {
		parts = append(parts, "SolarBeam "+d.EngineVersion)
	}
	line := strings.Join(parts, " · ")
	if line == "" {
		line = "Installed versions unavailable"
	}
	if u := strings.TrimSpace(d.UpdateStatus); u != "" && !strings.EqualFold(u, "unknown") {
		line += "\r\n" + u
	}
	return line
}

// editText converts text to the CRLF line breaks a multiline EDIT control
// needs; a bare LF shows as nothing and runs the lines together.
func editText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}
