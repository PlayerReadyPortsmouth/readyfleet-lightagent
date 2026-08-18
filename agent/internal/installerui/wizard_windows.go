//go:build windows

package installerui

import (
	"strings"
	"time"
	"unsafe"
)

// minStepDisplay is the shortest time each progress step's label stays on
// screen before RunWithProgress lets the next report() call replace it.
// The underlying install steps (hash a small file, write a registry value)
// are often faster than a human can read a sentence — this is a deliberate
// UX pace-limiter, not simulated work: report() still only ever fires for
// steps that actually happened, in the order they actually happened.
const minStepDisplay = 600 * time.Millisecond

// Wizard drives one installer's TaskDialog screens. Title is the window
// title shown on every screen (e.g. "ReadyFleet Light Agent Setup").
type Wizard struct {
	Title string
}

// ShowWelcome displays the opening screen: a main instruction, supporting
// content, an expandable "what will this be able to do" bullet list (shown
// expanded by default — a mentor installing something that talks to their
// own PC should see the permissions story without an extra click), and a
// footer with who to contact about it. Returns true only if the user
// clicked Continue; false for Cancel, the window close button, or Esc.
func (w Wizard) ShowWelcome(mainInstruction, content string, bullets []string, contact string) bool {
	expanded := "• " + strings.Join(bullets, "\n• ")
	buttons := []taskDialogButton{
		{nButtonID: idContinue, pszButtonText: utf16OrNil("Continue")},
		{nButtonID: idCancel, pszButtonText: utf16OrNil("Cancel")},
	}
	packedButtons := packTaskDialogButtons(buttons)
	cfg := &taskDialogConfig{
		dwFlags:                 tdfAllowDialogCancellation | tdfExpandedByDefault | tdfSizeToContent,
		pszWindowTitle:          utf16OrNil(w.Title),
		mainIcon:                tdInformationIcon,
		pszMainInstruction:      utf16OrNil(mainInstruction),
		pszContent:              utf16OrNil(content),
		cButtons:                uint32(len(buttons)),
		pButtons:                uintptr(unsafe.Pointer(&packedButtons[0])),
		nDefaultButton:          idContinue,
		pszExpandedInformation:  utf16OrNil(expanded),
		pszExpandedControlText:  utf16OrNil("Hide details"),
		pszCollapsedControlText: utf16OrNil("What will this be able to do?"),
		footerIcon:              tdInformationIcon,
		pszFooter:               utf16OrNil(contact),
	}
	buttonID, err := runTaskDialog(cfg, nil)
	if err != nil {
		return false
	}
	return buttonID == idContinue
}

// RunWithProgress shows a live progress screen while run executes on a
// background goroutine, then closes automatically the instant run
// returns — the dialog never waits for a click of its own. totalSteps is
// used only to compute the progress bar's fractional position; the actual
// step text comes from whatever label each report(label) call passes,
// since the real step sequence (lightinstall.Manager.Progress,
// bootstrap.Installer's equivalent) is driven by free-text descriptions
// decided at the call site, not a fixed enum this package would have to
// know about. report is safe to call from run's own goroutine (it always
// is run's goroutine — see below) and is a no-op once the dialog has
// closed.
//
// run's error is returned to the caller unmodified; this function does
// not itself decide what counts as success or show a result screen —
// that's ShowResult's job, deliberately kept separate so callers control
// the exact wording shown for their own failure modes.
func (w Wizard) RunWithProgress(totalSteps int, run func(report func(label string)) error) error {
	buttons := []taskDialogButton{
		{nButtonID: idOK, pszButtonText: utf16OrNil("Please wait…")},
	}
	packedButtons := packTaskDialogButtons(buttons)
	cfg := &taskDialogConfig{
		dwFlags:            tdfShowProgressBar | tdfSizeToContent,
		pszWindowTitle:     utf16OrNil(w.Title),
		mainIcon:           tdInformationIcon,
		pszMainInstruction: utf16OrNil("Installing…"),
		pszContent:         utf16OrNil("Starting…"),
		cButtons:           uint32(len(buttons)),
		pButtons:           uintptr(unsafe.Pointer(&packedButtons[0])),
		nDefaultButton:     idOK,
	}

	var runErr error
	runDone := make(chan struct{})

	onCreated := func(hwnd uintptr) {
		// Lock out the placeholder button the instant the dialog exists —
		// it exists only so runTaskDialog has something to click
		// programmatically to close the dialog; a user click must never
		// short-circuit an install mid-transaction.
		sendMessage(hwnd, tdmEnableButton, idOK, 0)
		sendMessage(hwnd, tdmSetProgressBarRange, 0, 0|(100<<16))

		stepCount := 0
		lastShown := time.Now()
		report := func(label string) {
			if elapsed := time.Since(lastShown); elapsed < minStepDisplay {
				time.Sleep(minStepDisplay - elapsed)
			}
			lastShown = time.Now()
			stepCount++
			pos := 100
			if totalSteps > 0 && stepCount < totalSteps {
				pos = stepCount * 100 / totalSteps
			}
			sendMessage(hwnd, tdmSetProgressBarPos, uintptr(pos), 0)
			setElementText(hwnd, tdeContent, label)
		}

		go func() {
			runErr = run(report)
			sendMessage(hwnd, tdmSetProgressBarPos, 100, 0)
			close(runDone)
			sendMessage(hwnd, tdmClickButton, idOK, 0)
		}()
	}

	_, _ = runTaskDialog(cfg, onCreated)
	<-runDone
	return runErr
}

// ShowResult displays the final screen. detail is shown verbatim — the
// caller is responsible for redacting anything sensitive first (mirrors
// bootstrap's existing sanitize() convention) — never collapsed to a
// bare success/failure flag, which is the entire point of this package:
// a mentor or venue operator should never be left looking at a closed
// console window with no idea what happened.
func (w Wizard) ShowResult(success bool, headline, detail string) {
	icon := uintptr(tdInformationIcon)
	if !success {
		icon = tdErrorIcon
	}
	buttons := []taskDialogButton{
		{nButtonID: idOK, pszButtonText: utf16OrNil("Close")},
	}
	packedButtons := packTaskDialogButtons(buttons)
	cfg := &taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation | tdfSizeToContent,
		pszWindowTitle:     utf16OrNil(w.Title),
		mainIcon:           icon,
		pszMainInstruction: utf16OrNil(headline),
		pszContent:         utf16OrNil(detail),
		cButtons:           uint32(len(buttons)),
		pButtons:           uintptr(unsafe.Pointer(&packedButtons[0])),
		nDefaultButton:     idOK,
	}
	_, _ = runTaskDialog(cfg, nil)
}

func setElementText(hwnd uintptr, element uint32, text string) {
	p := utf16OrNil(text)
	sendMessage(hwnd, tdmUpdateElementText, uintptr(element), uintptr(unsafe.Pointer(p)))
}

// idContinue is a custom button ID (values below 9 collide with the
// standard IDOK/IDCANCEL/etc. constants used by common buttons — 1000+ is
// the conventional safe range for custom TASKDIALOG_BUTTON IDs).
const idContinue = 1000
