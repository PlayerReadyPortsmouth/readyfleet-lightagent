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

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/staffmessage"
)

// NotifyPipePath carries staff messages to the companion. Like the mode pipe
// it is separate from the read-only status pipe, and it carries exactly
// staffmessage.Handler's two routes: collect the next message, and say
// whether it was shown.
const NotifyPipePath = `\\.\pipe\ReadyFleet.Venue.Notify.v1`

// notifyPollSlack is how much longer than the broker's long-poll the server
// and client allow a request to take.
const notifyPollSlack = 5 * time.Second

// ServeNotify serves the notify pipe for the selected CYP account: the status
// pipe's DACL, plus the mode pipe's per-request process check (token user is
// the SID, running in the active console session). SYSTEM never creates UI;
// the companion collects messages from here.
func ServeNotify(ctx context.Context, sid string, broker *staffmessage.Broker) error {
	if broker == nil {
		return errors.New("venue notify pipe needs a broker")
	}
	sddl, err := pipeSecurity(sid)
	if err != nil {
		return err
	}
	want, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	listener, err := winio.ListenPipe(NotifyPipePath, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		return fmt.Errorf("venue notify pipe: %w", err)
	}
	defer listener.Close()
	authorize := func(r *http.Request) error {
		conn, _ := r.Context().Value(connKey{}).(net.Conn)
		return authorizeClient(conn, want)
	}
	srv := &http.Server{
		Handler:           staffmessage.Handler(broker, authorize),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		// GET /v1/notify/next is a long-poll.
		WriteTimeout: broker.PollWait + notifyPollSlack, IdleTimeout: 3 * time.Second,
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

func notifyClient(timeout time.Duration) (*http.Client, func()) {
	transport := &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 4096, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := winio.DialPipeAccess(ctx, NotifyPipePath, windows.GENERIC_READ|windows.FILE_WRITE_DATA|windows.SYNCHRONIZE)
		if err != nil {
			return nil, err
		}
		if err := verifyServer(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("untrusted agent endpoint: %w", err)
		}
		return conn, nil
	}}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, transport.CloseIdleConnections
}

// StaffMessages is the companion's staffmessage.Client over the notify pipe.
type StaffMessages struct{}

// Next long-polls the service for one staff message.
func (StaffMessages) Next(ctx context.Context) (staffmessage.Message, bool, error) {
	client, done := notifyClient(staffmessage.DefaultPollWait + notifyPollSlack)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://readyfleet.local/v1/notify/next", nil)
	if err != nil {
		return staffmessage.Message{}, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return staffmessage.Message{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return staffmessage.Message{}, false, nil
	case http.StatusOK:
	default:
		return staffmessage.Message{}, false, fmt.Errorf("staff messages unavailable (%d)", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, staffmessage.MaxResponseBytes+1))
	if err != nil {
		return staffmessage.Message{}, false, err
	}
	if len(b) > staffmessage.MaxResponseBytes {
		return staffmessage.Message{}, false, errors.New("staff message exceeds size limit")
	}
	m, err := staffmessage.DecodeNext(b)
	if err != nil {
		return staffmessage.Message{}, false, err
	}
	return m, true, nil
}

// Ack tells the service whether a collected message reached the screen.
func (StaffMessages) Ack(ctx context.Context, a staffmessage.Ack) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	client, done := notifyClient(5 * time.Second)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://readyfleet.local/v1/notify/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("staff message ack refused (%d)", resp.StatusCode)
	}
	return nil
}
