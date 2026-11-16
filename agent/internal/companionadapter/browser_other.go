//go:build !windows

package companionadapter

import "errors"

func openNormalBrowser(string) error {
	return errors.New("native browser opening is not available on this platform")
}
