package solarbeam

import (
	"errors"
	"testing"
)

func TestHostingOffProcessScope(t *testing.T) {
	failure := errors.New("process inspection/stop failed")
	for _, tt := range []struct {
		name                         string
		mode                         Mode
		exactPath, owned             bool
		ownerErr, stopErr            error
		wantOwner, wantStop, wantErr bool
	}{
		{name: "managed exact install under console user", mode: ModeManaged, exactPath: true, wantStop: true},
		{name: "managed exact install under SYSTEM", mode: ModeManaged, exactPath: true, owned: true, wantStop: true},
		{name: "managed ignores owner inspection", mode: ModeManaged, exactPath: true, ownerErr: failure, wantStop: true},
		{name: "managed other installation untouched", mode: ModeManaged, owned: true},
		{name: "managed stop failure must not confirm off", mode: ModeManaged, exactPath: true, stopErr: failure, wantStop: true, wantErr: true},
		{name: "BYOD same owner", mode: ModeInteractive, exactPath: true, owned: true, wantOwner: true, wantStop: true},
		{name: "BYOD other owner untouched", mode: ModeInteractive, exactPath: true, wantOwner: true},
		{name: "BYOD owner unknown fails closed", mode: ModeInteractive, exactPath: true, ownerErr: failure, wantOwner: true, wantErr: true},
		{name: "BYOD other installation untouched", mode: ModeInteractive, owned: true},
		{name: "BYOD stop failure must not confirm off", mode: ModeInteractive, exactPath: true, owned: true, stopErr: failure, wantOwner: true, wantStop: true, wantErr: true},
		{name: "unknown mode cannot stop", mode: Mode(99), exactPath: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ownerCalled, stopCalled := false, false
			err := stopHostProcessInScope(tt.mode, tt.exactPath, func() (bool, error) { ownerCalled = true; return tt.owned, tt.ownerErr }, func() error { stopCalled = true; return tt.stopErr })
			if ownerCalled != tt.wantOwner || stopCalled != tt.wantStop || (err != nil) != tt.wantErr {
				t.Fatalf("owner=%v stop=%v err=%v", ownerCalled, stopCalled, err)
			}
		})
	}
}
