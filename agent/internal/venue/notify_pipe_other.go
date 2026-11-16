//go:build !windows

package venue

import (
	"context"
	"fmt"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/staffmessage"
)

func ServeNotify(context.Context, string, *staffmessage.Broker) error {
	return fmt.Errorf("managed venue staff messages require Windows")
}

// StaffMessages is the companion's staffmessage.Client; Windows-only.
type StaffMessages struct{}

func (StaffMessages) Next(context.Context) (staffmessage.Message, bool, error) {
	return staffmessage.Message{}, false, fmt.Errorf("managed venue staff messages require Windows")
}
func (StaffMessages) Ack(context.Context, staffmessage.Ack) error {
	return fmt.Errorf("managed venue staff messages require Windows")
}
