//go:build !windows

package companionui

import (
	"context"
	"errors"
)

func Run(context.Context, Backend, Options) error {
	return errors.New("native companion panel requires an interactive Windows session")
}
