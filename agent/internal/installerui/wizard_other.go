//go:build !windows

package installerui

import (
	"fmt"
	"strings"
)

// Wizard's non-Windows build is a plain console fallback — TaskDialog is
// a Windows-only API, and both real installers only ever run on Windows.
// This exists purely so the package (and anything that imports it, like
// a future cross-platform test harness) compiles cleanly everywhere,
// matching every other _windows.go/_other.go pair in this codebase.
type Wizard struct {
	Title string
}

func (w Wizard) ShowWelcome(mainInstruction, content string, bullets []string, contact string) bool {
	fmt.Println(w.Title)
	fmt.Println(mainInstruction)
	fmt.Println(content)
	fmt.Println("This will be able to:")
	for _, b := range bullets {
		fmt.Println("  - " + b)
	}
	if contact != "" {
		fmt.Println(contact)
	}
	return true
}

func (w Wizard) ShowConfirm(mainInstruction, content string) bool {
	fmt.Println(w.Title)
	fmt.Println(mainInstruction)
	fmt.Println(content)
	return true
}

func (w Wizard) RunWithProgress(totalSteps int, run func(report func(label string)) error) error {
	fmt.Println(w.Title)
	stepCount := 0
	return run(func(label string) {
		stepCount++
		fmt.Printf("[%d/%d] %s\n", stepCount, totalSteps, label)
	})
}

func (w Wizard) ShowResult(success bool, headline, detail string) {
	status := "OK"
	if !success {
		status = "FAILED"
	}
	fmt.Println(strings.ToUpper(status) + ": " + headline)
	if detail != "" {
		fmt.Println(detail)
	}
}
