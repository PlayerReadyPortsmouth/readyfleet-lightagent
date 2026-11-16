//go:build !windows

package installerui

import "fmt"

// PromptForCode's non-Windows build is a plain stdin fallback, matching
// wizard_other.go: the real installers only ever run on Windows, and this
// exists so the package compiles and stays testable everywhere. It splits
// caption from body the same way the Windows build does — w.Title is the
// heading, title/instruction are the message.
func (w Wizard) PromptForCode(title, instruction string) (string, bool) {
	fmt.Println(w.Title)
	fmt.Println(promptMessage(title, instruction))
	fmt.Print("Enter pairing code: ")
	var raw string
	if _, err := fmt.Scanln(&raw); err != nil {
		return "", false
	}
	return raw, true
}
