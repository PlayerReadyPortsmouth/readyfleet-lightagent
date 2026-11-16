//go:build windows

package venue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/modeswitch"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// ModePipePath is the mode picker's pipe. It is deliberately separate from
// the read-only status pipe (PipePath), whose protocol must never gain a
// mutation route. It carries exactly modeswitch.Handler's two routes.
const ModePipePath = `\\.\pipe\ReadyFleet.Venue.Mode.v1`

type connKey struct{}

// ServeMode serves the mode pipe for the selected CYP account. The DACL is
// the status pipe's (SYSTEM, Administrators, and limited access for that SID
// only; go-winio refuses remote clients), and every request is additionally
// authorised against the connecting process: its token user must be the SID
// and it must run in the active console session.
func ServeMode(ctx context.Context, sid string, svc *modeswitch.Service) error {
	sddl, err := pipeSecurity(sid)
	if err != nil {
		return err
	}
	want, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	listener, err := winio.ListenPipe(ModePipePath, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		return fmt.Errorf("venue mode pipe: %w", err)
	}
	defer listener.Close()
	authorize := func(r *http.Request) error {
		conn, _ := r.Context().Value(connKey{}).(net.Conn)
		return authorizeClient(conn, want)
	}
	srv := &http.Server{
		Handler: modeswitch.Handler(svc, authorize, ctx),
		// The PIN request waits for the C2 (bounded at 10 s by the service).
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 3 * time.Second,
		MaxHeaderBytes: 4096,
		ConnContext: func(c context.Context, conn net.Conn) context.Context {
			return context.WithValue(c, connKey{}, conn)
		},
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err = srv.Serve(&limitedListener{Listener: listener, slots: make(chan struct{}, 4)})
	if ctx.Err() != nil || err == http.ErrServerClosed {
		return nil
	}
	return err
}

// authorizeClient checks the pipe client's process: token user equal to the
// configured CYP SID, running in the active console session (never session 0).
func authorizeClient(conn net.Conn, want *windows.SID) error {
	if lc, ok := conn.(*limitedConn); ok {
		conn = lc.Conn
	}
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pipe client identity unavailable")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(fd.Fd()), &pid); err != nil || pid == 0 {
		return errors.New("pipe client process unavailable")
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil || session == 0 || session != windows.WTSGetActiveConsoleSessionId() {
		return errors.New("pipe client is not in the active console session")
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if !windows.EqualSid(user.User.Sid, want) {
		return errors.New("pipe client is not the configured CYP account")
	}
	return nil
}

func modeClient() (*http.Client, func()) {
	transport := &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 4096, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := winio.DialPipeAccess(ctx, ModePipePath, windows.GENERIC_READ|windows.FILE_WRITE_DATA|windows.SYNCHRONIZE)
		if err != nil {
			return nil, err
		}
		if err := verifyServer(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("untrusted agent endpoint: %w", err)
		}
		return conn, nil
	}}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, transport.CloseIdleConnections
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, modeswitch.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > modeswitch.MaxResponseBytes {
		return nil, errors.New("agent response exceeds size limit")
	}
	return b, nil
}

// ReadMode fetches the service's mode status as the interactive user.
func ReadMode(ctx context.Context) (modeswitch.Status, error) {
	client, done := modeClient()
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://readyfleet.local/v1/mode/status", nil)
	if err != nil {
		return modeswitch.Status{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return modeswitch.Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modeswitch.Status{}, fmt.Errorf("mode status unavailable (%d)", resp.StatusCode)
	}
	b, err := readLimited(resp.Body)
	if err != nil {
		return modeswitch.Status{}, err
	}
	var st modeswitch.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return modeswitch.Status{}, err
	}
	if st.Protocol != modeswitch.ProtocolVersion {
		return modeswitch.Status{}, errors.New("unsupported mode protocol")
	}
	return st, nil
}

// RequestMode sends the mode and PIN. The PIN is wiped, and the request body
// buffer with it, before RequestMode returns.
func RequestMode(ctx context.Context, mode string, pin *[proto.OperatingModePINDigits]byte) (modeswitch.Result, error) {
	defer clear(pin[:])
	if !proto.IsPickerOperatingMode(mode) {
		return modeswitch.Result{}, errors.New("unsupported mode")
	}
	body := make([]byte, 0, 64)
	body = append(body, `{"target_mode":"`...)
	body = append(body, mode...)
	body = append(body, `","pin":"`...)
	body = append(body, pin[:]...)
	body = append(body, `"}`...)
	defer clear(body)
	clear(pin[:])
	client, done := modeClient()
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://readyfleet.local/v1/mode/request", bytes.NewReader(body))
	if err != nil {
		return modeswitch.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return modeswitch.Result{}, err
	}
	defer resp.Body.Close()
	b, err := readLimited(resp.Body)
	if err != nil {
		return modeswitch.Result{}, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return modeswitch.Result{}, fmt.Errorf("mode request refused (%d)", resp.StatusCode)
	}
	var res modeswitch.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return modeswitch.Result{}, err
	}
	return res, nil
}
