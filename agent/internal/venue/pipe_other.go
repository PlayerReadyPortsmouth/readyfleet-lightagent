//go:build !windows

package venue

import (
	"context"
	"fmt"
)

func Serve(context.Context, string, *Store) error {
	return fmt.Errorf("managed venue companion requires Windows")
}
func Read(context.Context) (Snapshot, error) {
	return Snapshot{}, fmt.Errorf("managed venue companion requires Windows")
}
func ReadServices(names []string) []Service {
	out := make([]Service, 0, len(names))
	for _, name := range names {
		out = append(out, Service{Name: name, State: "unknown", StartType: "unknown"})
	}
	return out
}
func PostPrivacyOutcome(context.Context, PrivacyOutcome) error {
	return fmt.Errorf("managed venue companion requires Windows")
}
