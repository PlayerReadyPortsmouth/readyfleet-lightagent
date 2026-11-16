//go:build windows

package venue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const PipePath = `\\.\pipe\ReadyFleet.Venue.Status.v1`

// The selected account can exchange bytes but cannot create a competing pipe
// instance (FILE_CREATE_PIPE_INSTANCE overlaps FILE_APPEND_DATA). No Everyone or
// network-user ACE. go-winio also rejects remote clients at pipe creation.
func pipeSecurity(sidText string) (string, error) {
	sid, err := windows.StringToSid(sidText)
	if err != nil || sid == nil {
		return "", fmt.Errorf("venue companion requires a valid CYP SID")
	}
	if !(strings.HasPrefix(sid.String(), "S-1-5-21-") || strings.HasPrefix(sid.String(), "S-1-12-1-")) || sid.SubAuthorityCount() != 5 {
		return "", fmt.Errorf("venue companion requires an account SID")
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;" + sid.String() + ")", nil
}

func Serve(ctx context.Context, sid string, store *Store) error {
	sddl, err := pipeSecurity(sid)
	if err != nil {
		return err
	}
	listener, err := winio.ListenPipe(PipePath, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		return fmt.Errorf("venue pipe: %w", err)
	}
	defer listener.Close()
	srv := &http.Server{Handler: Handler(store), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second, MaxHeaderBytes: 4096}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err = srv.Serve(&limitedListener{Listener: listener, slots: make(chan struct{}, 8)})
	if ctx.Err() != nil || err == http.ErrServerClosed {
		return nil
	}
	return err
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }

// Authenticate the pipe server against SCM's running ReadyFleetAgent PID before
// sending a request. A user-created lookalike pipe must never become trusted UI
// status. Only SERVICE_QUERY_STATUS / SC_MANAGER_CONNECT are requested.
func verifyServer(conn net.Conn) error {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("pipe identity unavailable")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fd.Fd()), &pid); err != nil {
		return err
	}
	mgr, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(mgr)
	name, _ := windows.UTF16PtrFromString("ReadyFleetAgent")
	svc, err := windows.OpenService(mgr, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(svc)
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(svc, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed); err != nil {
		return err
	}
	if status.CurrentState != windows.SERVICE_RUNNING || pid == 0 || status.ProcessId != pid {
		return fmt.Errorf("pipe is not owned by the running ReadyFleet service")
	}
	return nil
}

func pipeClient() (*http.Client, func()) {
	transport := &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 4096, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := winio.DialPipeAccess(ctx, PipePath, windows.GENERIC_READ|windows.FILE_WRITE_DATA|windows.SYNCHRONIZE)
		if err != nil {
			return nil, err
		}
		if err := verifyServer(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("untrusted agent endpoint: %w", err)
		}
		return conn, nil
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, transport.CloseIdleConnections
}

func Read(ctx context.Context) (Snapshot, error) {
	client, closeIdle := pipeClient()
	defer closeIdle()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://readyfleet.local/v1/status", nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("agent status unavailable (%d)", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(b) > MaxResponseBytes {
		return Snapshot{}, fmt.Errorf("agent status exceeds size limit")
	}
	var v Snapshot
	if err := json.Unmarshal(b, &v); err != nil {
		return Snapshot{}, err
	}
	if err := v.Validate(time.Now()); err != nil {
		return Snapshot{}, err
	}
	return v, nil
}

// ReadServices queries exactly the reviewed service names. It has no stop,
// start, configuration, task or registry-write operations.
func ReadServices(names []string) []Service {
	values := make([]Service, 0, len(names))
	mgr, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err == nil {
		defer windows.CloseServiceHandle(mgr)
	}
	for _, name := range names {
		v := Service{Name: name, State: "unknown", StartType: "unknown"}
		if err == nil {
			queryService(mgr, name, &v)
		}
		values = append(values, v)
	}
	return values
}

func queryService(mgr windows.Handle, name string, v *Service) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return
	}
	h, err := windows.OpenService(mgr, n, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		if err == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			v.State = "not installed"
		}
		return
	}
	defer windows.CloseServiceHandle(h)
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed) == nil {
		switch status.CurrentState {
		case windows.SERVICE_STOPPED:
			v.State = "stopped"
		case windows.SERVICE_RUNNING:
			v.State = "running"
		default:
			v.State = "transitioning"
		}
	}
	buf := make([]byte, 8192)
	c := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if windows.QueryServiceConfig(h, c, uint32(len(buf)), &needed) == nil {
		switch c.StartType {
		case windows.SERVICE_AUTO_START:
			v.StartType = "automatic"
		case windows.SERVICE_DEMAND_START:
			v.StartType = "manual"
		case windows.SERVICE_DISABLED:
			v.StartType = "disabled"
		default:
			v.StartType = "other"
		}
	}
}

// PostPrivacyOutcome tells the service what privacy mode achieved. Best effort:
// the caller logs a failure and carries on.
func PostPrivacyOutcome(ctx context.Context, o PrivacyOutcome) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	client, closeIdle := pipeClient()
	defer closeIdle()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://readyfleet.local/v1/privacy-outcome", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("privacy outcome rejected (%d)", resp.StatusCode)
	}
	return nil
}
