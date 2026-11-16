//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionadapter"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func runLightCompanion(ctx context.Context, machineID string, client *transport.Client, mgr *solarbeam.Manager, openReadyApp func() error, tray *trayicon.Tray) (func(), bool) {
	hostname, _ := os.Hostname()
	broker, err := companionclient.New(companionclient.Config{DeviceID: machineID, DeviceName: hostname})
	if err != nil {
		broker = nil
	}
	local := func(ctx context.Context) (ui.Snapshot, error) {
		stats := client.Stats()
		st := mgr.Status(ctx)
		remote := mgr.RemotePresence(ctx)
		now := time.Now()
		conn := "Fleet disconnected"
		if stats.Connected {
			conn = "Fleet connected"
		}
		s := ui.Snapshot{ObservedAt: now, ValidUntil: now.Add(15 * time.Second), Device: ui.Device{ID: machineID, Name: hostname, Profile: "BYOD · local owner", Connection: conn, AgentVersion: agentVersion, EngineVersion: st.EngineVersion}, Authority: ui.Authority{HostingAuthorized: true}, Hosting: ui.Hosting{Known: st.HostingError == "", Available: st.HostingEnabled, Active: st.CurrentSessionID != "" || !remote.Known || remote.Count > 0, SessionID: st.CurrentSessionID, Revision: st.HostingRevision}, Details: st.HostingError}
		for _, v := range remote.Viewers {
			s.Hosting.ViewerNames = append(s.Hosting.ViewerNames, v.Name)
		}
		return s, nil
	}
	adapter := companionadapter.New(companionadapter.Options{Local: local, Client: broker, OpenApproval: companionadapter.OpenApprovalURL, OpenReadyApp: openReadyApp, OwnerHosting: func(ctx context.Context, action ui.Action) error {
		return mgr.SetHostingEnabledIfCurrent(ctx, action.Enabled, action.ExpectedRevision)
	}, OwnerDisconnect: func(ctx context.Context, action ui.Action) error {
		return mgr.StopIfCurrent(ctx, proto.StopSolarbeamArgs{SessionID: action.SessionID}, action.ExpectedRevision)
	}})
	prefs := filepath.Join(lightAppDataDir(), "companion-shortcuts.json")
	open := companionadapter.StartPanel(ctx, func(ctx context.Context, requests <-chan ui.OpenRequest) error {
		return ui.Run(ctx, adapter, ui.Options{Requests: requests, Shortcuts: companionadapter.LoadShortcuts(prefs), SaveShortcuts: func(b [4]ui.Shortcut) error { return companionadapter.SaveShortcuts(prefs, b) }})
	}, func(err error) { log.Printf("companion controls unavailable: %v", err) }, func() {
		_ = tray.Notify("ReadyFleet controls unavailable", "The protected controls panel could not open. Fleet management is still running. Check the agent log or restart ReadyFleet to retry.", trayicon.NotifyWarning, nil)
	})
	return open, true
}
