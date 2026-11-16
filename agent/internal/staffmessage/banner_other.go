//go:build !windows

package staffmessage

import (
	"context"
	"errors"
)

// BannerOptions wire the banner to the companion.
type BannerOptions struct {
	Queue   *Queue
	Wake    <-chan struct{}
	Balloon func(title, body, severity string) error
}

// RunBanner is Windows-only: the managed venue companion is a Windows app.
func RunBanner(context.Context, BannerOptions) error {
	return errors.New("the staff message banner requires an interactive Windows session")
}
