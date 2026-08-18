// Command lightagent is the restricted BYOD SolarBeam agent: it runs on a
// mentor's personally-owned Windows PC to host SolarBeam sessions, without
// any of the managed agent's shell/install/screen-relay/service-install
// capabilities. See
// docs/superpowers/specs/2026-07-26-all-venue-rollout-and-byod-light-agent-design.md
// and docs/superpowers/plans/2026-07-26-solarbeam-byod-light-agent.md.
//
// Hard import boundary (enforced by TestLightagentBinary_ContainsNoForbiddenStrings
// in safety_test.go, not just this package list): this package imports
// runtime, transport, enroll, config, lightinventory, lightinstall,
// solarbeam, update, and hiddenbrowser — never the managed agent/internal/exec,
// screen, terminal, service, inventory, or LAN-scan packages. It never
// registers a handler for shell, terminal, screen, lifecycle, launch,
// boot, unattended, discovery, wake, push-install, or generic install.
// hiddenbrowser introduces no new command kind — it's triggered purely
// locally from the tray menu, never over the wire.
//
// Runs as a current-user process launched from an HKCU Run key at logon,
// NOT a Windows service — svcpkg.IsWindowsService()/Run() are hard-wired
// to real SCM control and would misbehave here; only the plain
// file-logger helper is reused.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/enroll"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinventory"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/runtime"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	svcpkg "github.com/playerreadyportsmouth/readyfleet/agent/internal/svc"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/update"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// lightProductID and lightProfile identify this entrypoint's own releases
// to update.ProductPolicy. lightTrustedSigner is the same leaf
// code-signing fingerprint the managed agent uses (managedTrustedSigner,
// cmd/agent/main.go) — one leaf key signs every ReadyFleet Windows
// binary, confirmed live against codesign-service and the C2's own
// signing-trust bundle. CmdUpdate validates against it before ever
// downloading a candidate release.
const (
	lightProductID     = "readyfleet-light-agent"
	lightProfile       = "byod_solarbeam"
	lightTrustedSigner = "5b2e7596208e78bfd59d6e8d08844d2e3760614540040ca7f48e2dcb698be027"
)

var lightTrustedSigners = map[string]struct{}{lightTrustedSigner: {}}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "unlink":
			os.Exit(runUnlink(os.Args[2:]))
		case "uninstall":
			os.Exit(runUninstall(context.Background(), os.Args[2:]))
		}
	}

	var configPath string
	var insecure bool
	flag.StringVar(&configPath, "config", defaultLightConfigPath(), "path to light agent config")
	flag.BoolVar(&insecure, "insecure", false, "dev only: skip TLS verification")
	flag.Parse()

	logger, closer, err := svcpkg.OpenServiceLogger(defaultLightLogPath())
	if err != nil {
		// Falls back to stdout rather than exiting: unlike the managed
		// agent under SCM, a lightagent with no writable log directory
		// yet (first run, directory not created) should still be able to
		// enroll and connect.
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	} else {
		defer closer.Close()
	}
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		logger.Info("shutting down", "signal", sig.String())
		cancel()
	}()

	if err := runLightAgent(ctx, configPath, insecure, logger); err != nil {
		logger.Error("light agent exited", "err", err)
		os.Exit(1)
	}
	logger.Info("light agent stopped cleanly")
}

// runLightAgent mirrors agent/cmd/agent's runAgent lifecycle (load config,
// enrol if needed, connect and dispatch) but builds a tiny handler set
// instead of the managed agent's full manager roster.
func runLightAgent(ctx context.Context, configPath string, insecure bool, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("config load", "path", configPath, "err", err)
		return err
	}
	if cfg.ServerURL == "" {
		logger.Error("config missing server_url", "path", configPath)
		return fmt.Errorf("server_url required")
	}

	if !cfg.IsEnrolled() {
		if cfg.EnrollmentSecret == "" {
			logger.Error("light agent has no client cert and no enrollment secret to enrol with")
			return fmt.Errorf("not enrolled and no enrollment secret")
		}
		if err := runLightEnrollment(ctx, configPath, &cfg, logger, insecure); err != nil {
			logger.Error("enrollment failed", "err", err)
			return err
		}
	}

	solarbeamMgr := solarbeam.New(solarbeam.Paths{
		Engine: solarbeamPath("SOLARBEAM_ENGINE_PATH", lightEnginePath()),
		Bridge: solarbeamPath("SOLARBEAM_BRIDGE_PATH", lightBridgePath()),
		// Root is set explicitly rather than left for Update to derive from
		// filepath.Dir(Engine): the extraction target is the whole point of
		// this profile differing from managed, so it is stated, not inferred.
		Root: lightEngineDir(),
		// ModeInteractive: the mentor is already logged in interactively,
		// so this must not try to register a SYSTEM service (ModeManaged's
		// default) — verified live this session that doing so fails
		// outright without admin rights, which the light agent
		// deliberately never has. See the Mode type doc in
		// internal/solarbeam/manager.go.
		Mode: solarbeam.ModeInteractive,
	})

	updater := update.New(update.Config{
		AllowedHost: agentDownloadHost(cfg.ServerURL),
		Policy: update.ProductPolicy{
			ProductID: lightProductID,
			Profile:   lightProfile,
			Signers:   lightTrustedSigners,
		},
	})
	updater.ApplyFn = applyLightUpdate
	// A self-update restarts this process, which would kill the
	// ModeInteractive engine child it launched directly for an active
	// session — refuse while one is running, same guard the managed
	// agent wires (see cmd/agent/main.go).
	updater.SessionActiveFn = func(ctx context.Context) bool { return solarbeamMgr.Status(ctx).Running }

	logger.Info("light agent starting",
		"server_url", cfg.ServerURL,
		"machine_id", cfg.MachineID,
	)

	// Built before the handler map (not inside runTrayIcon, its previous
	// home) because CmdShowNotification's handler needs a live
	// *trayicon.Tray to call Notify on — see newLightTray's doc comment.
	tray, connectedIconPath, offlineIconPath := newLightTray()

	client := runtime.NewClient(runtime.Config{
		ServerURL:    cfg.ServerURL,
		CertFile:     cfg.CertPath,
		KeyFile:      cfg.KeyPath,
		CAFile:       cfg.CAPath,
		MachineID:    cfg.MachineID,
		AgentVersion: agentVersion,
		Insecure:     insecure,
		Logger:       logger,
		// The light entrypoint never sends a full InventoryData snapshot —
		// only LightInventoryData, via the CmdInventoryRefresh handler
		// below (MsgInventory's payload is opaque JSON, not locked to any
		// Go type, so this is legitimate). This callback exists only to
		// satisfy transport.Config's hello-triggered auto-refresh hook; an
		// empty snapshot leaks nothing since every field is zero-valued.
		// The C2's BYOD ingestion path (strict LightInventoryData decode,
		// Task 7) doesn't exist yet — this is a known, deliberate gap for
		// this increment, not a real inventory pipeline.
		InventoryProvider: func(ctx context.Context) (proto.InventoryData, error) {
			return proto.InventoryData{}, nil
		},
		Handlers: LightHandlers(solarbeamMgr, updater, tray),
	})

	// The tray icon polls client.Stats()/solarbeamMgr.Status() and drives
	// client.SetPaused() for its disconnect toggle — it needs the same
	// Client instance client.Run below ends up driving, not a separate
	// one runtime.Run would have hidden. A tray failure must never take
	// the real connection down with it, so this runs detached, own
	// goroutine, own error handling (logged inside runTrayIcon, never
	// propagated here).
	go runTrayIcon(ctx, tray, connectedIconPath, offlineIconPath, client, solarbeamMgr)

	return client.Run(ctx)
}

// runLightEnrollment redeems the enrollment secret and persists the
// returned material under configPath's directory (%LocalAppData%\ReadyFleet
// Light\). Sends only generic OS name/version in its inventory snapshot —
// no hostname or hardware fingerprint — mirroring the server's own
// BYOD privacy discard (server/internal/enroll/handler.go leaves those
// fields empty for byod_solarbeam regardless of what's sent).
func runLightEnrollment(ctx context.Context, configPath string, cfg *config.Config, logger *slog.Logger, insecure bool) error {
	enrollmentURL := cfg.EnrollmentURL
	if enrollmentURL == "" {
		enrollmentURL = deriveEnrollmentURL(cfg.ServerURL, insecure)
	}

	inv, err := lightinventory.Collect(ctx, agentVersion, nil)
	if err != nil {
		logger.Warn("initial light inventory collect failed; enrolling with minimal snapshot", "err", err)
	}

	materialDir := filepath.Dir(configPath)

	logger.Info("enrolling", "url", enrollmentURL)
	res, err := enroll.Enroll(ctx, enroll.Options{
		EnrollmentURL: enrollmentURL,
		BearerSecret:  cfg.EnrollmentSecret,
		AgentVersion:  agentVersion,
		Inventory: enroll.InventorySnapshot{
			OSName:    inv.OSFamily,
			OSVersion: inv.OSMajorVersion,
		},
		MaterialDir: materialDir,
		Insecure:    insecure,
	})
	if err != nil {
		return err
	}

	logger.Info("enrolled", "machine_id", res.MachineID)

	cfg.MachineID = res.MachineID
	cfg.VenueID = res.VenueID
	cfg.CertPath = res.CertPath
	cfg.KeyPath = res.KeyPath
	cfg.CAPath = res.CAPath
	if res.AgentListenURL != "" {
		cfg.ServerURL = res.AgentListenURL
	}
	cfg.EnrollmentSecret = "" // single-use: never let this machine re-enrol with the same secret

	return config.Save(configPath, *cfg)
}

// runUnlink removes local enrollment material and the current-user logon
// task, so the machine stops connecting AND stops silently retrying now-
// invalid credentials at every logon — a fresh enrollment secret is
// required to reconnect and re-register the task. Does NOT revoke the
// certificate server-side — that's the C2's job
// (POST /api/v1/byod-devices/:id/revoke, Task 7), triggered from the
// mentor's own "SolarBeam Devices" view, not from the machine itself.
//
// Also invoked as a subprocess by lightinstall.Manager.Uninstall (via
// `<agentExePath> unlink`) as its online-revoke step — this function must
// stay idempotent and safe to run with no interactive session, since that
// caller may run it non-interactively.
func runUnlink(args []string) int {
	path := defaultLightConfigPath()
	if len(args) > 0 {
		path = args[0]
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unlink: load config:", err)
		return 1
	}
	for _, p := range []string{cfg.CertPath, cfg.KeyPath, cfg.CAPath} {
		if p != "" {
			_ = os.Remove(p)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "unlink: remove config:", err)
		return 1
	}
	if err := lightinstall.RemoveLogonStartup(); err != nil {
		fmt.Fprintln(os.Stderr, "unlink: remove logon startup:", err)
		return 1
	}
	fmt.Println("unlinked")
	return 0
}

// runUninstall performs a full teardown: unlink (via lightinstall.Manager.
// Uninstall shelling back out to this same binary's `unlink` subcommand —
// see runUnlink's doc comment), remove the logon startup entry (idempotent second
// attempt if unlink already cleared it), then delete the entire install
// directory — INCLUDING this running lightagent.exe. Reuses lightinstall's
// orchestration rather than duplicating it, so light-installer.exe's
// Uninstall path (not yet wired to any caller — no uninstall UI trigger
// exists yet, flagged as follow-up) and this CLI entrypoint can never
// drift apart.
//
// NOT VERIFIED in this environment: deleting a currently-executing .exe's
// own directory relies on Windows' pending-delete semantics (the file is
// marked for removal and actually disappears once this process exits,
// rather than vanishing mid-run) — behavior that varies by Windows
// version and how the image was mapped, and needs confirming on a real
// host, not just asserted here.

func runUninstall(ctx context.Context, args []string) int {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "uninstall: locate self:", err)
		return 1
	}
	dir := filepath.Dir(exePath)
	mgr := lightinstall.New(lightinstall.Config{
		Paths: lightinstall.Paths{
			Root:       dir,
			AgentExe:   exePath,
			ConfigPath: defaultLightConfigPath(),
			KeyPath:    filepath.Join(dir, "client.key"),
		},
	})
	if err := mgr.Uninstall(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall failed:", err)
		return 1
	}
	fmt.Println("uninstalled")
	return 0
}

func defaultLightConfigPath() string {
	return filepath.Join(lightAppDataDir(), "config.json")
}

func defaultLightLogPath() string {
	return filepath.Join(lightAppDataDir(), "logs", "lightagent.log")
}

// lightAppDataDir is %LocalAppData%\ReadyFleet Light — current-user, no
// admin rights required, matching the design's "current-user background
// task" model (as opposed to the managed agent's %ProgramData% + LocalSystem
// service).
func lightAppDataDir() string {
	base := os.Getenv("LocalAppData")
	if base == "" {
		// Dev / fakeagent fallback, mirroring config.DefaultPath()'s own
		// non-Windows fallback convention.
		return "lightagent-data"
	}
	return filepath.Join(base, "ReadyFleet Light")
}

func stripSchemeAndPath(serverURL string) string {
	rest := serverURL
	for _, p := range []string{"wss://", "ws://", "https://", "http://"} {
		if strings.HasPrefix(rest, p) {
			rest = rest[len(p):]
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func deriveEnrollmentURL(serverURL string, insecure bool) string {
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	return scheme + "://" + stripSchemeAndPath(serverURL) + "/enroll/v1/redeem"
}

func agentDownloadHost(serverURL string) string {
	rest := stripSchemeAndPath(serverURL)
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// lightEngineDir is %LocalAppData%\SolarBeam — the BYOD engine's install
// root. Deliberately NOT the managed agent's C:\Program Files\SolarBeam:
// that directory can only be created with administrator rights, which this
// binary never has, so pointing here was not a preference but the difference
// between the engine installing and not. Observed live on 2026-08-18 before
// this changed, on a freshly enrolled BYOD machine:
//
//	solarbeam_engine_update failed:
//	create install dir: mkdir C:\Program Files\SolarBeam: Access is denied
//
// A sibling of lightAppDataDir() rather than a child of it: the agent
// replaces its own directory on self-update and must not carry the engine
// with it, and the engine arrives as its own release artifact
// (windows-engine-byod, a .zip precisely so it extracts without an
// installer). ModeInteractive already launches it from wherever it lands —
// see the Mode type doc in internal/solarbeam/manager.go.
func lightEngineDir() string {
	base := os.Getenv("LocalAppData")
	if base == "" {
		// Dev / fakeagent fallback, mirroring lightAppDataDir's.
		return "solarbeam-engine"
	}
	return filepath.Join(base, "SolarBeam")
}

func lightEnginePath() string { return filepath.Join(lightEngineDir(), "sunshine.exe") }

func lightBridgePath() string { return filepath.Join(lightEngineDir(), "solarbeam-bridge.exe") }

func solarbeamPath(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// agentVersion is set at build time via -ldflags. Default is "dev".
var agentVersion = "dev"
