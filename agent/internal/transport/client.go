// Package transport handles the agent's WebSocket connection to the C2.
//
// The Client maintains a single persistent connection with automatic
// reconnect. Each successful connection runs:
//
//   - the TLS dial (mTLS in production, plain WS in dev)
//   - MsgHello → MsgHelloAck handshake
//   - heartbeat loop (MsgPing from server → MsgPong from us)
//   - read pump dispatching messages to user-supplied handlers
//   - write pump draining a send channel
//   - a control-ping ticker + read deadline so a silently-dropped TCP
//     path surfaces as a ReadMessage error (see [readDeadline]) instead
//     of blocking the read pump forever
//
// On disconnect the loop sleeps with exponential backoff (1s..60s) and
// reconnects until ctx is cancelled.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/signedin"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Backoff is the exponential reconnect schedule, capped at 60s as per
// non-functional requirements.
type Backoff struct {
	Min time.Duration
	Max time.Duration
	cur time.Duration
}

// NewBackoff returns a Backoff starting at min, doubling up to max.
func NewBackoff(min, max time.Duration) *Backoff {
	if min <= 0 {
		min = time.Second
	}
	if max <= 0 || max < min {
		max = 60 * time.Second
	}
	return &Backoff{Min: min, Max: max}
}

// Next returns the next delay. The first call returns Min; each
// subsequent call doubles up to Max.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.Min
		return b.cur
	}
	b.cur *= 2
	if b.cur > b.Max {
		b.cur = b.Max
	}
	return b.cur
}

// Reset rewinds the backoff to its initial state. Call after a
// successful connection.
func (b *Backoff) Reset() { b.cur = 0 }

// Liveness bounds for the read pump. The server application-pings every
// proto.HeartbeatInterval (30s) and its gorilla stack auto-replies to our
// WebSocket control pings, so a healthy link refreshes the read deadline
// several times over; a silently-dropped TCP path stops refreshing it and
// ReadMessage returns an i/o timeout within readDeadline, which drives the
// reconnect backoff. Without this, ReadMessage blocks forever on a dead
// path and the documented 1s..60s reconnect never fires.
const (
	// readDeadline is how long ReadMessage may block before the link is
	// treated as dead. Set to 2.5× the 30s server heartbeat so a couple of
	// missed heartbeats don't flap a healthy connection.
	readDeadline = 75 * time.Second

	// pingInterval is how often the agent sends a WebSocket control ping.
	// The server heartbeats at the application level but never sends control
	// pings, so this is the agent's own liveness probe: the server's gorilla
	// stack auto-replies with a control pong that refreshes readDeadline.
	// Kept well under readDeadline.
	pingInterval = 25 * time.Second

	// defaultTelemetryInterval is how often the agent samples and pushes
	// MsgTelemetry when Config.TelemetryInterval is zero.
	defaultTelemetryInterval = 15 * time.Second
)

// Config configures a Client.
type Config struct {
	// ServerURL is the wss:// (or ws:// in dev) URL of the agent listener.
	ServerURL string

	// CertFile, KeyFile, CAFile are paths to mTLS material. CAFile is
	// the internal CA cert used to verify the server.
	CertFile string
	KeyFile  string
	CAFile   string

	// MachineID is the agent's machine_id, sent in the hello and (in
	// dev mode) added as X-Dev-Machine-Id when no client cert is in
	// use.
	MachineID string

	// AgentVersion identifies this build.
	AgentVersion string

	// OS, OSVersion populate the hello.
	OS        string
	OSVersion string

	// SACProbe, if non-nil, is read on every hello to report the Smart App
	// Control posture (see internal/sacposture). Read-only.
	SACProbe func() (state string, checkedAt time.Time)

	// SignedInProbe, if non-nil, is read on hello and on every telemetry
	// sample to report the interactive session's account (internal/signedin).
	SignedInProbe func() (user string, since time.Time)

	// AttendedProbe, if non-nil, derives TelemetryData.Attended from the
	// sample's signed-in report (internal/attended). Set only on the managed
	// Windows agent with the activity_signals kill switch on.
	AttendedProbe func(signedIn *proto.SignedInReport) *proto.AttendedReport

	// InventoryProvider, if non-nil, is called whenever the server
	// signals NeedsInventory in the hello_ack. The returned snapshot
	// is sent as a MsgInventory.
	InventoryProvider func(ctx context.Context) (any, error)

	// TelemetryProvider, if non-nil, is called every TelemetryInterval (or
	// defaultTelemetryInterval if zero) and the result pushed unsolicited as
	// a MsgTelemetry — unlike InventoryProvider, this isn't triggered by the
	// server, it's the agent's own fixed-cadence push (see the fleet
	// performance telemetry design spec's "always-on, single cadence"
	// decision).
	TelemetryProvider func(ctx context.Context) (proto.TelemetryData, error)

	// TelemetryInterval overrides the default cadence for TelemetryProvider.
	// Zero means defaultTelemetryInterval (15s).
	TelemetryInterval time.Duration

	// MachineEventsProvider, if non-nil, is called every
	// MachineEventsInterval to collect Windows event-log/WER deltas
	// (agent/internal/wineventlog). Returning (nil, nil) means "nothing
	// new" and skips the send entirely — most collections on a healthy
	// machine are empty.
	MachineEventsProvider func(ctx context.Context) (*proto.MachineEventsData, error)
	// MachineEventsInterval is the collection cadence (default 15m). The
	// caller should add a per-machine stagger offset before the first tick
	// so a venue of eight machines doesn't burst together.
	MachineEventsInterval time.Duration
	// MachineEventsStagger delays the FIRST collection after connect.
	MachineEventsStagger time.Duration

	// SecurityPostureProvider, if non-nil, is called every
	// SecurityPostureInterval (default 30m) to sample Defender / firewall /
	// Windows Update state (agent/internal/security). Unlike machine
	// events, posture is state, not deltas — every sample sends.
	SecurityPostureProvider func(ctx context.Context) (*proto.SecurityPostureData, error)
	SecurityPostureInterval time.Duration

	// HelloAckObserver, if non-nil, receives every hello_ack after the
	// handshake — the channel server-supplied observation config (e.g.
	// the Steam Family View keys) rides in on. Called on the dispatch
	// goroutine; implementations must be quick and thread-safe.
	HelloAckObserver func(ack proto.HelloAckData)
	// VenuePolicyObserver resets on reconnect and receives authenticated policy.
	VenuePolicyObserver func(*proto.VenuePolicy)

	// CommandHandler, if non-nil, receives every MsgCommand. It runs
	// in its own goroutine; the implementation is responsible for
	// streaming MsgOutput back via the provided Send func and for
	// emitting a final MsgResult.
	CommandHandler CommandHandler

	// TerminalHandler, if non-nil, receives the server→agent terminal_*
	// messages (open/input/resize/close). It dispatches them to the
	// agent's stateful TerminalManager.
	TerminalHandler TerminalHandler

	// OverrideDecisionHandler, if non-nil, receives server→agent
	// override_decision messages (software-control staff override replies).
	OverrideDecisionHandler func(ctx context.Context, env proto.Envelope)

	// ConsentDismissHandler, if non-nil, receives server→agent
	// consent_dismiss messages (owner decided in ReadyApp).
	ConsentDismissHandler func(ctx context.Context, env proto.Envelope)

	// OperatingModeGrantResponseHandler, if non-nil, receives server→agent
	// operating_mode_grant_response messages (answers to the venue PIN
	// requests this agent sent with [Client.Send]).
	OperatingModeGrantResponseHandler func(ctx context.Context, env proto.Envelope)

	// ComputeHandler, if non-nil, receives server→agent compute_lease,
	// compute_no_work and compute_cancel messages (ReadyCloudCompute).
	ComputeHandler func(ctx context.Context, env proto.Envelope)

	// ScreenHandler, if non-nil, receives the server→agent screen_start /
	// screen_stop messages and dispatches them to the agent's ScreenCapManager.
	ScreenHandler ScreenHandler

	// Logger is optional. Defaults to slog.Default().
	Logger *slog.Logger

	// Insecure, in dev only, skips client-cert presentation and
	// disables server-cert verification. Production must leave this
	// false.
	Insecure bool
}

// CommandHandler runs an incoming MsgCommand. Send is the callback that
// streams MsgOutput / MsgResult back through the same connection.
type CommandHandler func(ctx context.Context, cmd proto.Envelope, send SendFunc)

// TerminalHandler runs an incoming server→agent terminal_* message
// (open/input/resize/close). Like CommandHandler it runs in its own
// goroutine and uses the Send func to stream terminal_output / terminal_exit
// back through the same connection.
type TerminalHandler func(ctx context.Context, env proto.Envelope, send SendFunc)

// ScreenHandler runs an incoming server→agent screen_start or screen_stop
// message. It runs in its own goroutine and uses the Send func to emit
// screen_frame envelopes back through the same connection.
type ScreenHandler func(ctx context.Context, env proto.Envelope, send SendFunc)

// SendFunc enqueues an envelope on the active connection.
type SendFunc func(ctx context.Context, env proto.Envelope) error

// Stats is a point-in-time snapshot of the connection's health — cheap to
// poll from another goroutine (e.g. a tray icon showing live status);
// Client guards all of it with its own mutex, no separate synchronization
// needed by callers.
type Stats struct {
	Connected      bool
	ConnectedSince time.Time // zero value if not currently connected
	ConnectCount   int       // successful connections since Run started, first one included
	LastError      string    // most recent connectAndServe error, "" if none yet
}

// Client maintains the persistent WebSocket connection to the C2 with
// automatic reconnect.
type Client struct {
	cfg    Config
	logger *slog.Logger

	// sleepFn waits out one reconnect delay, reporting false if ctx was
	// cancelled first. A seam only so tests can observe the backoff
	// schedule without waiting out the real 1s..60s delays; production
	// always runs [sleepBackoff].
	sleepFn func(ctx context.Context, d time.Duration) bool

	mu            sync.Mutex
	running       bool
	stats         Stats
	paused        bool
	pauseWake     chan struct{}
	cancelCurrent context.CancelFunc
	// current is the live connection after hello, nil otherwise; Send uses it
	// for agent-initiated messages that are not replies to a command.
	current *persistentConn
}

// ErrNotConnected is returned by [Client.Send] while no connection is live.
var ErrNotConnected = errors.New("transport: not connected to the controller")

// Send queues an agent-initiated envelope (not a command reply) on the live
// connection. It never waits for a reconnect: a caller that needs an answer
// reports "not connected" and lets its user retry.
func (c *Client) Send(ctx context.Context, env proto.Envelope) error {
	c.mu.Lock()
	pc := c.current
	c.mu.Unlock()
	if pc == nil {
		return ErrNotConnected
	}
	return pc.Send(ctx, env)
}

// NewClient returns a Client. It does not start any I/O until [Client.Run].
func NewClient(cfg Config) *Client {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cfg:       cfg,
		logger:    logger,
		pauseWake: make(chan struct{}),
		sleepFn:   sleepBackoff,
	}
}

// sleepBackoff waits d out, returning false if ctx is cancelled first.
func sleepBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Stats returns a snapshot of the current connection state.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// SetPaused controls whether Run actively maintains a connection. Setting
// true drops any live or in-flight connection attempt immediately and
// stops the reconnect loop from dialing again until set back to false —
// the light agent tray icon's "disconnect" toggle needs exactly this,
// distinct from cancelling ctx (which stops the agent entirely). Setting
// false wakes a paused Run loop immediately rather than waiting out
// whatever backoff delay happened to be in progress when it was paused.
func (c *Client) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	if paused && c.cancelCurrent != nil {
		c.cancelCurrent()
	}
	close(c.pauseWake)
	c.pauseWake = make(chan struct{})
	c.mu.Unlock()
}

// Paused reports the current pause state set via [Client.SetPaused].
func (c *Client) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// ErrAlreadyRunning is returned by [Client.Run] if it is invoked twice.
var ErrAlreadyRunning = errors.New("transport: client already running")

// Run drives the reconnect loop until ctx is cancelled. Returns nil on
// graceful shutdown.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return ErrAlreadyRunning
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	backoff := NewBackoff(time.Second, 60*time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		c.mu.Lock()
		paused, wake := c.paused, c.pauseWake
		c.mu.Unlock()
		if paused {
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
			}
			continue
		}
		err := c.connectAndServe(ctx)
		c.mu.Lock()
		c.stats.Connected = false
		c.stats.ConnectedSince = time.Time{}
		if err != nil {
			c.stats.LastError = err.Error()
		}
		c.mu.Unlock()
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		if errors.Is(err, errPausedMidConnection) {
			// SetPaused cancelled a live connection on purpose — that's
			// not a real failure, so skip the backoff delay and let the
			// pause-check at the top of the loop take over immediately.
			continue
		}
		delay := backoff.Next()
		c.logger.Info("agent reconnect scheduled",
			"err", err, "delay", delay.String())
		if !c.sleepFn(ctx, delay) {
			return nil
		}
	}
}

// errPausedMidConnection distinguishes a SetPaused(true)-triggered
// cancellation from a genuine connection failure — see its one use in Run.
var errPausedMidConnection = errors.New("transport: paused")

func (c *Client) connectAndServe(ctx context.Context) (retErr error) {
	// attemptCtx wraps ctx for this whole attempt — dial included — so
	// SetPaused can cancel a connection that's still mid-dial, not just an
	// already-established one. cancelCurrent is cleared and attemptCtx's
	// cancellation reinterpreted as errPausedMidConnection (rather than a
	// generic failure feeding the reconnect backoff) whenever the OUTER
	// ctx is still alive — that combination only happens via SetPaused.
	attemptCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancelCurrent = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.cancelCurrent = nil
		c.mu.Unlock()
		// Read attemptCtx's state BEFORE cancelling it. cancel() makes
		// Err() non-nil unconditionally, so testing it afterwards would
		// call every failure a pause and skip the reconnect backoff —
		// turning a refused dial into a hot loop. Clearing cancelCurrent
		// above already means no later SetPaused can cancel this attempt,
		// so this read sees exactly the pause that raced the failure.
		abortedMidAttempt := attemptCtx.Err() != nil
		cancel()
		if retErr != nil && ctx.Err() == nil && abortedMidAttempt {
			retErr = errPausedMidConnection
		}
	}()

	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}

	headers := http.Header{}
	if c.cfg.Insecure && c.cfg.MachineID != "" {
		headers.Set("X-Dev-Machine-Id", c.cfg.MachineID)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  tlsCfg,
	}

	parsed, err := url.Parse(c.cfg.ServerURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}

	c.logger.Info("agent dialing", "url", c.cfg.ServerURL)
	conn, _, err := dialer.DialContext(attemptCtx, parsed.String(), headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	c.logger.Info("agent connected", "url", c.cfg.ServerURL)

	pc := newPersistentConn(conn, c.logger)
	connCtx := attemptCtx

	// Send hello.
	if c.cfg.VenuePolicyObserver != nil {
		c.cfg.VenuePolicyObserver(nil)
	}
	hello := proto.HelloData{
		VenuePolicySupported: true,
		ProtocolVersion:      proto.ProtocolVersion,
		AgentVersion:         c.cfg.AgentVersion,
		MachineID:            c.cfg.MachineID,
		OS:                   c.cfg.OS,
		OSVersion:            c.cfg.OSVersion,
		SignedIn:             signedin.Report(c.cfg.SignedInProbe),
	}
	if c.cfg.SACProbe != nil {
		state, at := c.cfg.SACProbe()
		hello.SACState, hello.SACCheckedAt = state, &at
	}
	helloEnv, err := proto.NewEnvelope(proto.MsgHello, uuid.Nil, nil, hello)
	if err != nil {
		return err
	}
	if err := pc.Send(connCtx, helloEnv); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	c.mu.Lock()
	c.stats.Connected = true
	c.stats.ConnectedSince = time.Now()
	c.stats.ConnectCount++
	c.current = pc
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.current == pc {
			c.current = nil
		}
		c.mu.Unlock()
	}()

	// Run pumps.
	readErr := make(chan error, 1)
	go func() { readErr <- pc.readLoop(connCtx, c.dispatch) }()
	writeErr := make(chan error, 1)
	go func() { writeErr <- pc.writeLoop(connCtx) }()
	pingErr := make(chan error, 1)
	go func() { pingErr <- pc.pingLoop(connCtx) }()
	telemetryErr := make(chan error, 1)
	go func() { telemetryErr <- c.telemetryLoop(connCtx, pc) }()
	machineEventsErr := make(chan error, 1)
	go func() { machineEventsErr <- c.machineEventsLoop(connCtx, pc) }()
	postureErr := make(chan error, 1)
	go func() { postureErr <- c.securityPostureLoop(connCtx, pc) }()

	select {
	case err := <-readErr:
		return err
	case err := <-writeErr:
		return err
	case err := <-pingErr:
		return err
	case err := <-telemetryErr:
		return err
	case err := <-machineEventsErr:
		return err
	case err := <-postureErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispatch routes one incoming envelope.
func (c *Client) dispatch(ctx context.Context, pc *persistentConn, env proto.Envelope) error {
	switch env.Type {
	case proto.MsgHelloAck:
		var ack proto.HelloAckData
		if err := env.DecodeData(&ack); err != nil {
			return err
		}
		c.logger.Info("agent handshake complete",
			"protocol_version", ack.AcceptedProtocolVersion,
			"server_time", ack.ServerTime,
			"needs_inventory", ack.NeedsInventory,
		)
		if c.cfg.VenuePolicyObserver != nil {
			c.cfg.VenuePolicyObserver(ack.VenuePolicy)
		}
		if c.cfg.HelloAckObserver != nil {
			c.cfg.HelloAckObserver(ack)
		}
		if ack.NeedsInventory && c.cfg.InventoryProvider != nil {
			go c.sendInventory(ctx, pc)
		}
		return nil

	case proto.MsgPing:
		pong, err := proto.NewEnvelope(proto.MsgPong, uuid.Nil, &env.ID, nil)
		if err != nil {
			return err
		}
		return pc.Send(ctx, pong)

	case proto.MsgCommand:
		if c.cfg.CommandHandler == nil {
			c.logger.Warn("ignoring command (no handler installed)",
				"id", env.ID)
			return nil
		}
		go c.cfg.CommandHandler(ctx, env, pc.Send)
		return nil

	case proto.MsgTerminalOpen, proto.MsgTerminalInput, proto.MsgTerminalResize, proto.MsgTerminalClose:
		if c.cfg.TerminalHandler == nil {
			c.logger.Warn("ignoring terminal message (no handler installed)", "type", env.Type)
			return nil
		}
		go c.cfg.TerminalHandler(ctx, env, pc.Send)
		return nil

	case proto.MsgScreenStart, proto.MsgScreenStop, proto.MsgViewingStarted, proto.MsgViewingStopped:
		if c.cfg.ScreenHandler == nil {
			c.logger.Warn("ignoring screen message (no handler installed)", "type", env.Type)
			return nil
		}
		go c.cfg.ScreenHandler(ctx, env, pc.Send)
		return nil

	case proto.MsgOverrideDecision:
		if c.cfg.OverrideDecisionHandler != nil {
			go c.cfg.OverrideDecisionHandler(ctx, env)
		}
		return nil

	case proto.MsgConsentDismiss:
		if c.cfg.ConsentDismissHandler != nil {
			go c.cfg.ConsentDismissHandler(ctx, env)
		}
		return nil

	case proto.MsgOperatingModeGrantResponse:
		if c.cfg.OperatingModeGrantResponseHandler != nil {
			go c.cfg.OperatingModeGrantResponseHandler(ctx, env)
		}
		return nil

	case proto.MsgComputeLease, proto.MsgComputeNoWork, proto.MsgComputeCancel:
		if c.cfg.ComputeHandler != nil {
			go c.cfg.ComputeHandler(ctx, env)
		}
		return nil

	case proto.MsgCancel:
		// Cancellation routing lives next to the command runtime in a
		// later commit. For now we log and drop.
		c.logger.Info("received cancel", "in_reply_to", env.InReplyTo)
		return nil

	default:
		c.logger.Warn("agent unknown message type", "type", env.Type)
		return nil
	}
}

func (c *Client) sendInventory(ctx context.Context, pc *persistentConn) {
	if c.cfg.InventoryProvider == nil {
		return
	}
	inv, err := c.cfg.InventoryProvider(ctx)
	if err != nil {
		c.logger.Warn("inventory collection failed", "err", err)
		return
	}
	env, err := proto.NewEnvelope(proto.MsgInventory, uuid.Nil, nil, inv)
	if err != nil {
		c.logger.Warn("inventory envelope", "err", err)
		return
	}
	if err := pc.Send(ctx, env); err != nil {
		c.logger.Warn("inventory send", "err", err)
	}
}

// telemetryLoop calls c.cfg.TelemetryProvider on a fixed interval and sends
// each result as MsgTelemetry, until ctx is cancelled or a send fails. A
// provider error for one tick is logged and skipped — it does not stop the
// loop, since a transient WMI/sensor hiccup shouldn't tear down the
// connection the way a genuine transport error should. Returns ctx.Err()
// (not nil) on cancellation, matching pingLoop/writeLoop's convention —
// connectAndServe's deferred SetPaused-vs-real-failure detection depends on
// every pumped goroutine returning a non-nil error when its ctx is done, so
// a nil return here would let a paused-mid-connection cancellation look
// like connectAndServe (and therefore Run) exiting cleanly, ending the
// reconnect loop for good instead of pausing it.
func (c *Client) telemetryLoop(ctx context.Context, pc *persistentConn) error {
	if c.cfg.TelemetryProvider == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	interval := c.cfg.TelemetryInterval
	if interval <= 0 {
		interval = defaultTelemetryInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		data, err := c.cfg.TelemetryProvider(ctx)
		if err != nil {
			c.logger.Warn("telemetry collection failed", "err", err)
			continue
		}
		data.SignedIn = signedin.Report(c.cfg.SignedInProbe)
		if c.cfg.AttendedProbe != nil {
			data.Attended = c.cfg.AttendedProbe(data.SignedIn)
		}
		env, err := proto.NewEnvelope(proto.MsgTelemetry, uuid.Nil, nil, data)
		if err != nil {
			c.logger.Warn("telemetry envelope", "err", err)
			continue
		}
		if err := pc.Send(ctx, env); err != nil {
			return err
		}
	}
}

// machineEventsLoop runs the event-log/WER collector on its interval,
// mirroring telemetryLoop's conventions (non-nil return on ctx cancel; a
// provider error logs and skips the tick). Two differences: an initial
// stagger delay so venue machines don't collect in lockstep, and a nil
// batch from the provider (nothing new) is silently skipped.
func (c *Client) machineEventsLoop(ctx context.Context, pc *persistentConn) error {
	if c.cfg.MachineEventsProvider == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	interval := c.cfg.MachineEventsInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if d := c.cfg.MachineEventsStagger; d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		data, err := c.cfg.MachineEventsProvider(ctx)
		if err != nil {
			c.logger.Warn("machine events collection failed", "err", err)
		} else if data != nil {
			env, err := proto.NewEnvelope(proto.MsgMachineEvents, uuid.Nil, nil, data)
			if err != nil {
				c.logger.Warn("machine events envelope", "err", err)
			} else if err := pc.Send(ctx, env); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// securityPostureLoop samples and sends security posture on its interval,
// with an immediate first sample after connect (posture staleness across a
// reconnect is exactly when the server most wants a fresh read — e.g. a
// machine coming back after sitting off for a week).
func (c *Client) securityPostureLoop(ctx context.Context, pc *persistentConn) error {
	if c.cfg.SecurityPostureProvider == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	interval := c.cfg.SecurityPostureInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		data, err := c.cfg.SecurityPostureProvider(ctx)
		if err != nil {
			c.logger.Warn("security posture collection failed", "err", err)
		} else if data != nil {
			env, err := proto.NewEnvelope(proto.MsgSecurityPosture, uuid.Nil, nil, data)
			if err != nil {
				c.logger.Warn("security posture envelope", "err", err)
			} else if err := pc.Send(ctx, env); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) tlsConfig() (*tls.Config, error) {
	if c.cfg.Insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	if c.cfg.CertFile == "" || c.cfg.KeyFile == "" || c.cfg.CAFile == "" {
		return nil, errors.New("transport: cert/key/ca paths required when not in insecure mode")
	}
	cert, err := tls.LoadX509KeyPair(c.cfg.CertFile, c.cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	caBytes, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("transport: CA file did not contain a usable certificate")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// --- per-connection state --------------------------------------------------

type persistentConn struct {
	conn   *websocket.Conn
	send_  chan []byte
	logger *slog.Logger

	// readDeadline / pingInterval default to the package consts; tests
	// override them to prove the stalled-reader and healthy-idle paths
	// without waiting the full production window.
	readDeadline time.Duration
	pingInterval time.Duration

	closeOnce sync.Once
	closed    chan struct{}
}

func newPersistentConn(conn *websocket.Conn, logger *slog.Logger) *persistentConn {
	return &persistentConn{
		conn:         conn,
		send_:        make(chan []byte, 32),
		logger:       logger,
		readDeadline: readDeadline,
		pingInterval: pingInterval,
		closed:       make(chan struct{}),
	}
}

// Send queues env for delivery. Errors if the connection has been closed.
func (p *persistentConn) Send(ctx context.Context, env proto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	select {
	case <-p.closed:
		return errors.New("transport: connection closed")
	case <-ctx.Done():
		return ctx.Err()
	case p.send_ <- b:
		return nil
	}
}

func (p *persistentConn) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return p.conn.Close()
}

func (p *persistentConn) readLoop(ctx context.Context, on func(context.Context, *persistentConn, proto.Envelope) error) error {
	p.conn.SetReadLimit(2 * 1024 * 1024)
	// Arm the read deadline and refresh it whenever the peer proves the link
	// is alive — via a control pong (below) or any inbound message (in the
	// loop). A silently-dropped path stops refreshing it and ReadMessage
	// returns a timeout within p.readDeadline, surfacing the dead link.
	_ = p.conn.SetReadDeadline(time.Now().Add(p.readDeadline))
	p.conn.SetPongHandler(func(string) error {
		return p.conn.SetReadDeadline(time.Now().Add(p.readDeadline))
	})
	for {
		_, raw, err := p.conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = p.conn.SetReadDeadline(time.Now().Add(p.readDeadline))
		var env proto.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			p.logger.Warn("agent malformed inbound", "err", err)
			continue
		}
		if err := on(ctx, p, env); err != nil {
			p.logger.Warn("agent dispatch", "type", env.Type, "err", err)
		}
	}
}

// pingLoop sends a WebSocket control ping every p.pingInterval. gorilla's
// WriteControl is safe to call concurrently with the write pump's
// WriteMessage, so this needs no coordination with writeLoop. The server's
// gorilla stack auto-replies with a control pong, which the read pump's
// SetPongHandler uses to refresh the read deadline — keeping a healthy but
// idle link alive independently of the server's application-level heartbeat.
func (p *persistentConn) pingLoop(ctx context.Context) error {
	t := time.NewTicker(p.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closed:
			return nil
		case <-t.C:
			if err := p.conn.WriteControl(
				websocket.PingMessage, nil, time.Now().Add(10*time.Second),
			); err != nil {
				return err
			}
		}
	}
}

func (p *persistentConn) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closed:
			return nil
		case b := <-p.send_:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return err
			}
		}
	}
}
