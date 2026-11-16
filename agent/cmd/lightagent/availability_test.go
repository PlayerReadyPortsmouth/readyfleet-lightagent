package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestRemoteBYODAvailabilityCannotEnableHosting(t *testing.T) {
	dir := t.TempDir()
	manager := solarbeam.New(solarbeam.Paths{Mode: solarbeam.ModeInteractive, StateDir: dir, Engine: filepath.Join(dir, "uninstalled-engine"), Bridge: filepath.Join(dir, "uninstalled-bridge")})
	for _, raw := range []string{`{"enabled":true}`, `{}`, `{"enabled":null}`} {
		env, err := proto.NewEnvelope(proto.MsgCommand, uuid.New(), nil, proto.CommandData{Kind: proto.CmdSetSolarbeamAvailability, Args: json.RawMessage(raw)})
		if err != nil {
			t.Fatal(err)
		}
		result := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
			runSetSolarbeamAvailability(context.Background(), env, transport.SendFunc(send), manager)
		})
		if result.ExitCode == 0 {
			t.Fatalf("BYOD accepted %s", raw)
		}
	}
}
