//go:build windows

package companionadapter

import (
	"errors"
	"golang.org/x/sys/windows"
)

// Both approval and ReadyApp use ShellExecute's normal browser/profile.
// The browser window is not covered by the companion panel's capture exclusion.
func openNormalBrowser(raw string) error {
	p, err := windows.UTF16PtrFromString(raw)
	if err != nil {
		return errors.New("invalid browser URL")
	}
	if err = windows.ShellExecute(0, nil, p, nil, nil, 1); err != nil {
		return errors.New("could not open ReadyApp in the browser")
	}
	return nil
}
