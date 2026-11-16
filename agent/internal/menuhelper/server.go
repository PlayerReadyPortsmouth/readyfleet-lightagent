package menuhelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
)

// DefaultTimeout is how long a prompt waits for an answer in the menu bar
// before the silence counts as a decline: the same two minutes the shared
// connection prompt allows (connectionprompt.AnswerTimeout).
const DefaultTimeout = 2 * time.Minute

// maxLine bounds one JSON line from the helper. Decisions are tiny; anything
// larger is not this protocol.
const maxLine = 64 * 1024

// Server is the agent's end of the socket. One helper connection is active
// at a time: a newer one replaces the old, which is what happens when the
// helper is relaunched. Safe for concurrent use.
type Server struct {
	path   string
	ln     net.Listener
	logger *slog.Logger

	// Timeout bounds Ask; zero means DefaultTimeout.
	Timeout time.Duration
	// NewID mints prompt ids; tests pin it.
	NewID func() string

	mu      sync.Mutex
	conn    *helperConn
	status  *Status
	pending map[string]chan Decision
}

// helperConn is one accepted connection with a channel closed when it ends,
// so an Ask in flight can tell the helper went away mid-question.
type helperConn struct {
	net.Conn
	done    chan struct{}
	writeMu sync.Mutex
}

// Listen creates the socket at path, owner-only (0600 on the socket, 0700 on
// a directory it has to create), replacing a stale file left by a crashed
// agent. The listener is not served until Serve is called.
func Listen(path string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("menuhelper: socket dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("menuhelper: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("menuhelper: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("menuhelper: chmod socket: %w", err)
	}
	return &Server{path: path, ln: ln, logger: logger, pending: map[string]chan Decision{}}, nil
}

// Path is the socket path, for the helper's command line.
func (s *Server) Path() string { return s.path }

// Serve accepts helper connections until ctx ends, then closes the listener
// and removes the socket file. It never returns an error: a broken menu must
// not take the agent's connection down with it.
func (s *Server) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("menu helper: accept", "err", err)
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
			}
			break
		}
		hc := &helperConn{Conn: c, done: make(chan struct{})}
		s.mu.Lock()
		old := s.conn
		s.conn = hc
		s.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		go s.read(hc)
	}
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	_ = os.Remove(s.path)
}

// Close stops the listener; use when Serve is not running under a context.
func (s *Server) Close() error { return s.ln.Close() }

// Connected reports whether a helper is attached right now.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

func (s *Server) read(hc *helperConn) {
	defer func() {
		close(hc.done)
		_ = hc.Close()
		s.mu.Lock()
		if s.conn == hc {
			s.conn = nil
		}
		s.mu.Unlock()
		s.logger.Info("menu helper disconnected")
	}()
	sc := bufio.NewScanner(hc)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	for sc.Scan() {
		msg, err := Decode(sc.Bytes())
		if err != nil {
			var unknown *ErrUnknownType
			if errors.As(err, &unknown) {
				s.logger.Debug("menu helper: ignoring message", "type", unknown.Type)
				continue
			}
			s.logger.Warn("menu helper: bad line; dropping the connection", "err", err)
			return
		}
		switch m := msg.(type) {
		case Hello:
			s.logger.Info("menu helper connected", "helper_version", m.Version)
			s.mu.Lock()
			st := s.status
			s.mu.Unlock()
			if st != nil {
				s.send(hc, *st)
			}
		case Decision:
			s.mu.Lock()
			ch := s.pending[m.ID]
			s.mu.Unlock()
			if ch == nil {
				s.logger.Warn("menu helper: decision for a prompt that is not open", "id", m.ID)
				continue
			}
			select {
			case ch <- m:
			default: // already answered
			}
		default:
			s.logger.Debug("menu helper: unexpected message from helper", "type", fmt.Sprintf("%T", msg))
		}
	}
}

func (s *Server) send(hc *helperConn, msg any) {
	b, err := Encode(msg)
	if err != nil {
		s.logger.Warn("menu helper: encode", "err", err)
		return
	}
	hc.writeMu.Lock()
	defer hc.writeMu.Unlock()
	_ = hc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := hc.Write(b); err != nil {
		s.logger.Warn("menu helper: write", "err", err)
		_ = hc.Close()
	}
}

// sendCurrent writes to whichever helper is attached, if any.
func (s *Server) sendCurrent(msg any) bool {
	s.mu.Lock()
	hc := s.conn
	s.mu.Unlock()
	if hc == nil {
		return false
	}
	s.send(hc, msg)
	return true
}

// Publish records the status (so a helper connecting later gets it) and
// pushes it to the attached helper.
func (s *Server) Publish(st Status) {
	st.Type = TypeStatus
	s.mu.Lock()
	s.status = &st
	s.mu.Unlock()
	s.sendCurrent(st)
}

// Ask puts a connection request to the person through the menu bar and
// reports the answer. It implements consent.Asker, so its contract is that
// contract: OutcomeAccepted only when somebody pressed Approve; every other
// route out is a decline naming the reason. The helper gets the request's
// title and body (which ReadyApp writes to name who is asking) and the
// deadline; it never gets the URL or the requester's id.
func (s *Server) Ask(ctx context.Context, req consent.Request) consent.Outcome {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	newID := s.NewID
	if newID == nil {
		newID = uuid.NewString
	}

	s.mu.Lock()
	hc := s.conn
	s.mu.Unlock()
	if hc == nil {
		s.logger.Warn("consent prompt not shown: the menu bar helper is not connected")
		return consent.OutcomeNoPrompter
	}

	id := newID()
	ch := make(chan Decision, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	deadline := time.Now().Add(timeout)
	s.send(hc, Prompt{ID: id, Requester: req.Title, Detail: req.Body, ExpiresAt: deadline.UTC()})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-ch:
		if d.Approve {
			return consent.OutcomeAccepted
		}
		s.logger.Warn("consent prompt declined at the device (menu bar)")
		return consent.OutcomeDeclined
	case <-hc.done:
		s.logger.Warn("consent prompt abandoned: the menu bar helper disconnected while waiting for an answer")
		return consent.OutcomeNoPrompter
	case <-timer.C:
		s.send(hc, Cancel{ID: id})
		s.logger.Warn("consent prompt timed out with no answer (menu bar)", "after", timeout.String())
		return consent.OutcomeTimedOut
	case <-ctx.Done():
		s.send(hc, Cancel{ID: id})
		s.logger.Warn("consent prompt abandoned: the agent stopped waiting", "err", ctx.Err())
		return consent.OutcomeCancelled
	}
}

// LastStatus returns the most recently published status, nil before the
// first Publish. For the agent's own tests; the helper gets it via hello.
func (s *Server) LastStatus() *Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		return nil
	}
	st := *s.status
	return &st
}
