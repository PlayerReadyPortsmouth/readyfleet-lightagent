//go:build windows

package installerui

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modcredui                       = windows.NewLazySystemDLL("credui.dll")
	procCredUIPromptForCredentialsW = modcredui.NewProc("CredUIPromptForCredentialsW")
)

// credUIInfo mirrors CREDUI_INFOW (wincred.h) — a naturally-aligned struct
// (uint32 + 4 bytes of tail padding, then three 8-byte members), unlike
// TASKDIALOGCONFIG, so no manual byte-packing is required here. That is
// the whole reason this prompt is built on CredUI rather than a
// hand-rolled DialogBoxIndirectParamW template with an Edit control: this
// package has already been burned once by TASKDIALOGCONFIG's byte-packed
// C layout (see taskdialog_windows.go's marshal() comment — a
// naturally-aligned Go struct produced a silent E_INVALIDARG on every
// call). A DLGTEMPLATE is the same class of hand-marshaled, offset-exact
// binary blob, and only buys chrome that matches the TaskDialog screens.
// The trade accepted here is visual: this is the standard Windows
// credential prompt, not a TaskDialog, so it does not look identical to
// the wizard's other screens.
type credUIInfo struct {
	cbSize         uint32
	hwndParent     uintptr
	pszMessageText *uint16
	pszCaptionText *uint16
	hbmBanner      uintptr
}

const (
	// wincred.h CREDUI_FLAGS_*. KEEP_USERNAME (0x00100000) is deliberately
	// NOT set: it makes the user-name field read-only, which would make it
	// impossible to type the code into the only field this flow uses.
	// VALIDATE_USERNAME / COMPLETE_USERNAME are likewise omitted — they
	// impose domain\user syntax on a field holding a pairing code.
	creduiFlagsDoNotPersist       = 0x00000002
	creduiFlagsAlwaysShowUI       = 0x00000080
	creduiFlagsGenericCredentials = 0x00040000

	// CREDUI_MAX_USERNAME_LENGTH / CREDUI_MAX_PASSWORD_LENGTH. The API
	// rejects buffer sizes above these with ERROR_INVALID_PARAMETER.
	credUIMaxUsernameLength = 513
	credUIMaxPasswordLength = 256

	errorCancelled = 1223 // ERROR_CANCELLED — user dismissed the prompt.
)

// pairingCodeCredTarget is CredUIPromptForCredentialsW's pszTargetName,
// which is mandatory (a NULL or empty target fails with
// ERROR_INVALID_PARAMETER before any UI is shown). Nothing is persisted
// under it — CREDUI_FLAGS_DO_NOT_PERSIST is always set — so it serves
// only as the prompt's subject line.
const pairingCodeCredTarget = "ReadyFleet"

// PromptForCode shows a native Windows credential-style prompt asking for
// the pairing code, using the code as the "user name" field (the only
// field this flow needs); the password field is shown but left blank and
// its contents are ignored. ok is false if the user cancelled or closed
// the prompt, mirroring ShowWelcome's bool-return convention. The raw
// typed value is NOT normalized here — callers should run it through
// NormalizePairingCode.
//
// Caption vs. body follows the same split as every other screen in this
// package: the dialog's caption is always w.Title (the one window title
// ShowWelcome, RunWithProgress, and ShowResult all use, e.g. "ReadyFleet
// Agent Setup"), and the title/instruction arguments are the message body,
// joined with a blank line — CredUI exposes only the single
// pszMessageText, so headline and detail share it.
//
// Verified against the real API on Windows 11 (26200) rather than from
// documentation alone, since the argument list below is hand-marshaled:
// the prompt renders as a standard #32770 dialog with User name / Password
// fields and OK / Cancel; clicking OK after typing into the user-name
// field returns 0 and hands that text back through pszUserName, and Cancel
// returns ERROR_CANCELLED.
//
// One behaviour callers must handle: OK stays ENABLED with both fields
// empty, and an empty submission returns ("", true) — a blank entry is
// NOT reported as a cancel. That is why validation belongs to the caller
// via NormalizePairingCode, which rejects "" like any other malformed
// input; ok only answers "did the user dismiss the prompt", never "is
// this a usable code".
func (w Wizard) PromptForCode(title, instruction string) (code string, ok bool) {
	captionPtr, err := windows.UTF16PtrFromString(w.Title)
	if err != nil {
		return "", false
	}
	msgPtr, err := windows.UTF16PtrFromString(promptMessage(title, instruction))
	if err != nil {
		return "", false
	}
	targetPtr, err := windows.UTF16PtrFromString(pairingCodeCredTarget)
	if err != nil {
		return "", false
	}
	info := credUIInfo{
		pszCaptionText: captionPtr,
		pszMessageText: msgPtr,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	usernameBuf := make([]uint16, credUIMaxUsernameLength)
	passwordBuf := make([]uint16, credUIMaxPasswordLength)
	var save int32 // BOOL out-param, unused (creduiFlagsDoNotPersist set)

	// Argument list is CredUIPromptForCredentialsW's real 10-parameter
	// wincred.h signature: pUiInfo, pszTargetName, pContext, dwAuthError,
	// pszUserName, ulUserNameBufferSize, pszPassword,
	// ulPasswordBufferSize, *save, dwFlags.
	ret, _, _ := procCredUIPromptForCredentialsW.Call(
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(targetPtr)),
		0, // pContext — SSPI context, unused for generic credentials
		0, // dwAuthError — no prior failure to report
		uintptr(unsafe.Pointer(&usernameBuf[0])),
		uintptr(len(usernameBuf)),
		uintptr(unsafe.Pointer(&passwordBuf[0])),
		uintptr(len(passwordBuf)),
		uintptr(unsafe.Pointer(&save)),
		uintptr(creduiFlagsGenericCredentials|creduiFlagsAlwaysShowUI|creduiFlagsDoNotPersist),
	)
	// Zero whatever the user typed into the password field: it is never
	// used, and leaving it in a live heap buffer serves no purpose.
	for i := range passwordBuf {
		passwordBuf[i] = 0
	}
	if ret != 0 {
		// NO_ERROR is the only success; ERROR_CANCELLED (1223) and every
		// other failure are the same non-answer to the caller.
		return "", false
	}
	return windows.UTF16ToString(usernameBuf), true
}
