//go:build !windows

package venue

import (
	"context"
	"fmt"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/modeswitch"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func ServeMode(context.Context, string, *modeswitch.Service) error {
	return fmt.Errorf("managed venue mode picker requires Windows")
}
func ReadMode(context.Context) (modeswitch.Status, error) {
	return modeswitch.Status{}, fmt.Errorf("managed venue mode picker requires Windows")
}
func RequestMode(_ context.Context, _ string, pin *[proto.OperatingModePINDigits]byte) (modeswitch.Result, error) {
	clear(pin[:])
	return modeswitch.Result{}, fmt.Errorf("managed venue mode picker requires Windows")
}
