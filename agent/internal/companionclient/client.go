// Package companionclient implements only the scoped ReadyApp companion broker.
// Pairing/native credentials stay in process memory. It cannot accept general
// API bearer tokens, browser cookies, arbitrary paths or redirected origins.
package companionclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const DefaultOrigin = "https://readyapp.player-ready.co.uk"
const apiPrefix = "/api/solarbeam/companion"

var tokenPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var codePattern = regexp.MustCompile(`^[A-F0-9]{8}$`)
var errorPattern = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)
var ErrSignedOut = errors.New("Staff controls are signed out")
var ErrBrowserUnavailable = errors.New("Approving ReadyApp browser is unavailable")
var ErrPairingUncertain = errors.New("Approval response was not confirmed; cancel and restart sign-in")

type responseError struct {
	status int
	code   string
}

func (e *responseError) Error() string { return "ReadyApp companion: " + e.code }

type Config struct {
	Origin, DeviceID, DeviceName string
	HTTPClient                   *http.Client
}
type Pairing struct {
	UserCode, VerificationURL string
	ExpiresAt                 time.Time
}
type pendingPair struct {
	id, secret string
	uncertain  bool
	public     Pairing
}
type Client struct {
	mu                           sync.Mutex
	origin, deviceID, deviceName string
	http                         *http.Client
	token                        string
	expires                      time.Time
	pair                         *pendingPair
}

func (c *Client) String() string   { return "ReadyApp companion client (credentials redacted)" }
func (c *Client) GoString() string { return c.String() }

type Session struct {
	RegistrationID      string    `json:"registrationId"`
	Revision            string    `json:"revision"`
	SessionID           string    `json:"sessionId"`
	MachineID           string    `json:"machineId"`
	HostName            string    `json:"hostName"`
	ParticipantIdentity string    `json:"participantIdentity"`
	ParticipantSID      string    `json:"participantSid"`
	AllowedActions      []string  `json:"allowedActions"`
	ExclusiveHeldBySelf bool      `json:"exclusiveHeldBySelf"`
	ActivityID          string    `json:"activityId"`
	CYPContactID        string    `json:"cypContactId"`
	CYPName             string    `json:"cypName"`
	ActivityLabel       string    `json:"activityLabel"`
	ActivityStartsAt    time.Time `json:"activityStartsAt"`
	ActivityEndsAt      time.Time `json:"activityEndsAt"`
}
type Command struct {
	CommandID string          `json:"commandId"`
	Status    string          `json:"status"`
	Error     string          `json:"error"`
	Result    json.RawMessage `json:"result"`
}
type State struct {
	HostMutationPending bool      `json:"hostMutationPending"`
	DeviceID            string    `json:"deviceId"`
	DeviceName          string    `json:"deviceName"`
	StaffName           string    `json:"staffName"`
	ExpiresAt           time.Time `json:"expiresAt"`
	HostAllowedActions  []string  `json:"hostAllowedActions"`
	Sessions            []Session `json:"sessions"`
	Commands            []Command `json:"commands"`
}
type Admission struct {
	ActivityID   string `json:"activityId"`
	CYPContactID string `json:"cypContactId"`
}
type Action struct {
	RegistrationID    string     `json:"registrationId,omitempty"`
	ExpectedRevision  string     `json:"expectedRevision,omitempty"`
	IdempotencyKey    string     `json:"idempotencyKey"`
	Action            string     `json:"action"`
	Admission         *Admission `json:"admission,omitempty"`
	Enabled           *bool      `json:"enabled,omitempty"`
	ExpectedSessionID string     `json:"expectedSessionId,omitempty"`
}

func New(cfg Config) (*Client, error) {
	origin := cfg.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("companion requires a fixed HTTPS origin")
	}
	if _, err := uuid.Parse(cfg.DeviceID); err != nil {
		return nil, errors.New("companion requires enrolled device identity")
	}
	name := strings.TrimSpace(cfg.DeviceName)
	if name == "" {
		name = "ReadyFleet device"
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return nil, errors.New("invalid device name")
		}
	}
	h := &http.Client{Timeout: 10 * time.Second}
	if cfg.HTTPClient != nil {
		*h = *cfg.HTTPClient
	}
	h.Jar = nil
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("companion redirects are refused") }
	if h.Timeout == 0 || h.Timeout > 10*time.Second {
		h.Timeout = 10 * time.Second
	}
	return &Client{origin: strings.TrimRight(origin, "/"), deviceID: cfg.DeviceID, deviceName: name, http: h}, nil
}

// request sends with the current credential (when auth) and forgets it on an
// authority refusal. Callers hold c.mu.
func (c *Client) request(ctx context.Context, method, path string, body any, auth bool, out any) error {
	token := ""
	if auth {
		if c.token == "" {
			return ErrSignedOut
		}
		token = c.token
	}
	err := c.send(ctx, method, path, body, token, out)
	var refusal *responseError
	if auth && errors.As(err, &refusal) && (refusal.status == 401 || refusal.status == 403 || refusal.status == 410) && c.token == token {
		c.token = ""
		c.expires = time.Time{}
	}
	return err
}

// send performs one broker request with an explicit credential (empty = none).
// It never touches the client's stored credential, so it is safe without c.mu.
func (c *Client) send(ctx context.Context, method, path string, body any, token string, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("invalid companion payload")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+apiPrefix+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid companion request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-ReadyFleet-Companion", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("ReadyApp companion connection unavailable")
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(buf) > 1024*1024 {
		return errors.New("invalid companion response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(buf, &e)
		if !errorPattern.MatchString(e.Error) {
			e.Error = "request_refused"
		}
		if e.Error == "browser_unavailable" && (resp.StatusCode == 503 || resp.StatusCode == 409) {
			return ErrBrowserUnavailable
		}
		return &responseError{status: resp.StatusCode, code: e.Error}
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(buf, &envelope) != nil || len(envelope.Data) == 0 {
		return errors.New("invalid companion response")
	}
	if out != nil && json.Unmarshal(envelope.Data, out) != nil {
		return errors.New("invalid companion data")
	}
	return nil
}
func (c *Client) BeginPairing(ctx context.Context) (Pairing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pair != nil && time.Now().Before(c.pair.public.ExpiresAt) {
		return c.pair.public, nil
	}
	c.token = ""
	c.expires = time.Time{} // Explicit re-pair clears prior local authority.
	var p struct {
		PairingID, PairingSecret, UserCode, VerificationPath string
		ExpiresAt                                            time.Time
	}
	if err := c.request(ctx, http.MethodPost, "/pairings", map[string]string{"deviceId": c.deviceID, "deviceName": c.deviceName}, false, &p); err != nil {
		return Pairing{}, err
	}
	if _, err := uuid.Parse(p.PairingID); err != nil || !tokenPattern.MatchString(p.PairingSecret) || !codePattern.MatchString(p.UserCode) || p.VerificationPath != "/solarbeam/companion/connect?pairing="+p.PairingID || !time.Now().Before(p.ExpiresAt) {
		return Pairing{}, errors.New("invalid pairing response")
	}
	c.pair = &pendingPair{id: p.PairingID, secret: p.PairingSecret, public: Pairing{p.UserCode, c.origin + p.VerificationPath, p.ExpiresAt}}
	return c.pair.public, nil
}
func (c *Client) Pairing() *Pairing {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pair == nil || !time.Now().Before(c.pair.public.ExpiresAt) {
		return nil
	}
	p := c.pair.public
	return &p
}

// HasBinding reports only whether a bounded credential remains in memory;
// it does not grant authority while the browser or broker is unavailable.
func (c *Client) HasBinding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != "" && time.Now().Before(c.expires)
}
func (c *Client) State(ctx context.Context) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" && c.pair != nil {
		if c.pair.uncertain {
			return State{}, ErrPairingUncertain
		}
		if time.Now().After(c.pair.public.ExpiresAt) {
			c.pair = nil
			return State{}, ErrSignedOut
		}
		var p struct {
			Status, Token string
			ExpiresAt     time.Time
		}
		err := c.request(ctx, http.MethodPost, "/pairings/"+c.pair.id+"/poll", map[string]string{"pairingSecret": c.pair.secret}, false, &p)
		if err != nil {
			var refusal *responseError
			if errors.As(err, &refusal) && (refusal.status == 410 || refusal.code == "pairing_consumed") {
				c.pair = nil
				return State{}, ErrSignedOut
			}
			// A poll can consume the one-use token before its response is lost.
			// Never auto-replay that credential after an ambiguous outcome.
			c.pair.uncertain = true
			return State{}, ErrPairingUncertain
		}
		if p.Status == "pending" {
			return State{}, ErrSignedOut
		}
		if p.Status != "approved" || !tokenPattern.MatchString(p.Token) || !time.Now().Before(p.ExpiresAt) {
			c.pair.uncertain = true
			return State{}, ErrPairingUncertain
		}
		c.token = p.Token
		c.expires = p.ExpiresAt
		c.pair = nil
	}
	if c.token == "" || time.Now().After(c.expires) {
		c.token = ""
		return State{}, ErrSignedOut
	}
	if time.Until(c.expires) < 2*time.Minute {
		var renewal struct{ ExpiresAt time.Time }
		if err := c.request(ctx, http.MethodPost, "/native/renew", nil, true, &renewal); err != nil {
			return State{}, err
		}
		if !time.Now().Before(renewal.ExpiresAt) {
			c.token = ""
			return State{}, ErrSignedOut
		}
		c.expires = renewal.ExpiresAt
	}
	var state State
	if err := c.request(ctx, http.MethodGet, "/native/state", nil, true, &state); err != nil {
		return State{}, err
	}
	if state.DeviceID != c.deviceID || len(state.Sessions) > 8 || len(state.Commands) > 64 || !time.Now().Before(state.ExpiresAt) {
		return State{}, errors.New("companion state identity or expiry mismatch")
	}
	seen := map[string]bool{}
	for _, s := range state.Sessions {
		if _, err := uuid.Parse(s.RegistrationID); err != nil || seen[s.RegistrationID] || !tokenPattern.MatchString(s.Revision) || s.ParticipantSID == "" {
			return State{}, errors.New("invalid companion session binding")
		}
		if _, err := uuid.Parse(s.SessionID); err != nil {
			return State{}, errors.New("invalid companion session identity")
		}
		seen[s.RegistrationID] = true
	}
	return state, nil
}
func (c *Client) Action(ctx context.Context, action Action) (Command, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out Command
	if _, err := uuid.Parse(action.IdempotencyKey); err != nil {
		return out, errors.New("action requires idempotency identity")
	}
	switch action.Action {
	case "take", "release", "admit", "focus", "leave":
		if _, err := uuid.Parse(action.RegistrationID); err != nil || !tokenPattern.MatchString(action.ExpectedRevision) {
			return out, errors.New("action requires exact browser registration")
		}
		if action.Enabled != nil || action.ExpectedSessionID != "" {
			return out, errors.New("unexpected host action fields")
		}
		if action.Action == "admit" && (action.Admission == nil || action.Admission.ActivityID == "" || action.Admission.CYPContactID == "") {
			return out, errors.New("admission requires exact person and activity")
		}
	case "set_hosting", "disconnect_hosting", "check_updates":
		if action.RegistrationID != "" || action.ExpectedRevision != "" || action.Admission != nil {
			return out, errors.New("host action cannot target a viewing session")
		}
		if action.Action == "set_hosting" && action.Enabled == nil {
			return out, errors.New("explicit hosting target required")
		}
	default:
		return out, errors.New("unsupported companion action")
	}
	if err := c.request(ctx, http.MethodPost, "/native/actions", action, true, &out); err != nil {
		return out, err
	}
	if _, err := uuid.Parse(out.CommandID); err != nil {
		return Command{}, errors.New("invalid command receipt")
	}
	return out, nil
}
func (c *Client) SignOut(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.token = ""; c.pair = nil; c.expires = time.Time{} }()
	if c.token == "" {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/native/link", nil, true, nil)
}

// Revoke erases the native credential and any pending pairing immediately,
// without waiting on the network, and reports whether anything was held. The
// remote unlink is best-effort and runs in the background with the captured
// credential: a session lock must not stall the panel's message loop on a slow
// broker, and local authority is already gone whether or not it succeeds.
func (c *Client) Revoke() bool {
	c.mu.Lock()
	token := c.token
	held := token != "" || c.pair != nil
	c.token = ""
	c.pair = nil
	c.expires = time.Time{}
	c.mu.Unlock()
	if token != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = c.send(ctx, http.MethodDelete, "/native/link", nil, token, nil)
		}()
	}
	return held
}

// DeviceID is public binding metadata, never a credential.
func (c *Client) DeviceID() string { return c.deviceID }
