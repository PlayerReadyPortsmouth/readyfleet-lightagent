package lightinstall

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/enroll"
)

// Failure keeps diagnostics out of the user-facing instruction. Rollback failures
// remain visible: setup must never claim it left a clean machine when cleanup failed.
type Failure struct {
	Step     string
	Code     string
	Cause    error
	Rollback error
	LogLines []string
}

func (f *Failure) Error() string {
	return fmt.Sprintf("installer %s (%s): %v; rollback: %v", f.Step, f.Code, f.Cause, f.Rollback)
}
func (f *Failure) Unwrap() error { return f.Cause }

func (f *Failure) Instruction() string {
	next := map[string]string{
		"preflight":   "Check the PC clock, free disk space and your internet connection. Allow ReadyFleet through your proxy or firewall, then try again.",
		"stage":       "Close ReadyFleet and try again. Check Windows Security Protection History for a blocked file; ask support to check the signature before allowing it.",
		"verify":      "Download a fresh setup file from ReadyApp. If Windows Security blocked it, ask support to check its signature. Do not switch off protection.",
		"enroll":      "Open ReadyApp and download a fresh setup link: the previous code may have expired or already been used. Confirm the device when ReadyApp asks.",
		"credentials": "Check that your account can write to its own app-data folder, then download a fresh setup link.",
		"startup":     "Check your account's startup-app policy with support, then run setup again.",
		"start":       "Check Windows Security Protection History or your Linux user service, then run setup again.",
		"connect":     "Allow ReadyFleet's encrypted connection through your firewall or proxy, then run setup again.",
	}
	if f.Code == "owner_mismatch" {
		return "This PC is already set up for a different ReadyApp account. Ask that owner to remove it from their device list, then download a fresh setup link for your account."
	}
	if f.Code == "setup_running" {
		return "Another ReadyFleet setup is already running. Close it or wait for it to finish, then try again."
	}
	instruction := next[f.Step]
	if instruction == "" {
		instruction = "Run setup again, or contact support with the step and error code below."
	}
	if f.Rollback != nil {
		instruction += " Cleanup could not finish. Contact support before trying again."
	}
	return "Setup stopped at " + f.Step + " (" + f.Code + "). " + instruction
}

type savedFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	present bool
}

func snapshot(path string) (savedFile, error) {
	f := savedFile{path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if !info.Mode().IsRegular() {
		return f, fmt.Errorf("install path is not a regular file")
	}
	f.data, err = os.ReadFile(path)
	f.mode = info.Mode().Perm()
	f.present = true
	return f, err
}
func restoreFile(f savedFile) error {
	if !f.present {
		err := os.Remove(f.path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return atomicFile(f.path, f.data, f.mode)
}
func atomicFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".install-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func stageAgent(path string, data []byte) error { return atomicFile(path, data, 0755) }

// Reuse only a complete identity for this controller. The certificate/key must
// match, remain valid and chain to its enrolled CA. An arbitrary config file is
// not evidence that a token can safely be skipped.
func reusableIdentity(cfg Config) bool {
	c, err := config.Load(cfg.Paths.ConfigPath)
	if err != nil || c.MachineID == "" || c.EnrollmentSecret != "" {
		return false
	}
	expected, _ := url.Parse(cfg.ServerURL)
	actual, _ := url.Parse(c.ServerURL)
	if expected == nil || actual == nil || !strings.EqualFold(expected.Hostname(), actual.Hostname()) {
		return false
	}
	if filepath.Clean(c.KeyPath) != filepath.Clean(cfg.Paths.KeyPath) || filepath.Clean(c.CertPath) != filepath.Join(cfg.Paths.Root, "client.crt") || filepath.Clean(c.CAPath) != filepath.Join(cfg.Paths.Root, "ca.crt") {
		return false
	}
	pair, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || cert.Subject.CommonName != c.MachineID {
		return false
	}
	ca, err := os.ReadFile(c.CAPath)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err == nil
}

func (m *Manager) installResilient(ctx context.Context, payload Payload) (result error) {
	m.freshConnection = true
	release, err := acquireInstallLock(m.cfg.Paths.Root)
	if err != nil {
		return &Failure{Step: "preflight", Code: "setup_running", Cause: err}
	}
	defer release()
	fail := func(step string, err error) error { return &Failure{Step: step, Code: step + "_failed", Cause: err} }
	m.progress("Checking this PC and the connection…")
	if err := m.preflightFn(ctx, m.cfg); err != nil {
		return fail("preflight", err)
	}
	// Snapshot only installer-owned files. Existing logs and other user files are
	// never recursively deleted on repair. A new install's own directory is removed.
	_, rootErr := os.Stat(m.cfg.Paths.Root)
	newRoot := os.IsNotExist(rootErr)
	var saved []savedFile
	for _, p := range []string{m.cfg.Paths.AgentExe, m.cfg.Paths.ConfigPath, m.cfg.Paths.KeyPath, filepath.Join(m.cfg.Paths.Root, "client.crt"), filepath.Join(m.cfg.Paths.Root, "ca.crt")} {
		f, err := snapshot(p)
		if err != nil {
			return fail("stage", err)
		}
		saved = append(saved, f)
	}
	reused := reusableIdentity(m.cfg)
	if reused {
		c, _ := config.Load(m.cfg.Paths.ConfigPath)
		if payload.ExpectedOwnerActorID == "" || !strings.EqualFold(c.OwnerActorID, payload.ExpectedOwnerActorID) {
			return &Failure{Step: "enroll", Code: "owner_mismatch", Cause: fmt.Errorf("existing install belongs to a different ReadyApp account; unlink it in ReadyApp before enrolling another owner")}
		}
	}
	var restoreStartup func() error
	if m.snapshotStartupFn != nil {
		var err error
		restoreStartup, err = m.snapshotStartupFn()
		if err != nil {
			return fail("startup", err)
		}
	}
	stopped := false
	startupTouched := false
	enrolled := false
	defer func() {
		if result == nil {
			return
		}
		var failure *Failure
		if !errors.As(result, &failure) {
			return
		}
		failure.LogLines = tailLog(m.cfg.Paths.LogPath, 12)
		if !stopped {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cleanup []error
		// Unlink clears only the new local identity. The controller retains its
		// machine record; we never unlink a pre-existing identity.
		if enrolled && !reused {
			_ = m.unlinkOnlineFn(cleanupCtx, m.cfg.Paths.AgentExe)
		}
		if stopped {
			cleanup = append(cleanup, m.stopAgentFn(m.cfg.Paths.AgentExe))
		}
		if startupTouched && !reused && restoreStartup == nil {
			cleanup = append(cleanup, m.removeTaskFn())
		}
		for _, f := range saved {
			restored := restoreFile(f)
			cleanup = append(cleanup, restored)
			if restored == nil && f.present && f.path == m.cfg.Paths.KeyPath {
				cleanup = append(cleanup, m.protectKeyFn(f.path))
			}
		}
		if startupTouched && restoreStartup != nil {
			cleanup = append(cleanup, restoreStartup())
		}
		// A retry checkpoint belongs to this failed attempt. Keep no bootstrap
		// secret/private key in an otherwise rolled-back installation.
		cleanup = append(cleanup, removeIfPresent(filepath.Join(m.cfg.Paths.Root, ".enrollment-recovery.json")))
		cleanup = append(cleanup, removeIfPresent(filepath.Join(m.cfg.Paths.Root, ".enrollment-recovery.json.tmp")))
		if newRoot {
			cleanup = append(cleanup, m.removeAllFn(m.cfg.Paths.Root))
		}
		if reused && stopped {
			if restoreStartup == nil {
				cleanup = append(cleanup, m.registerTaskFn(m.cfg.Paths.AgentExe))
			}
			cleanup = append(cleanup, m.startAgentFn(m.cfg.Paths.AgentExe))
		}
		failure.Rollback = errors.Join(cleanup...)
	}()
	if err := m.stopAgentFn(m.cfg.Paths.AgentExe); err != nil {
		return fail("stage", err)
	}
	stopped = true
	m.progress("Staging and verifying the agent…")
	if len(m.cfg.AgentBytes) > 0 {
		if err := m.stageAgentFn(m.cfg.Paths.AgentExe, m.cfg.AgentBytes); err != nil {
			return fail("stage", err)
		}
	}
	if err := m.verifyAgentFn(m.cfg.Paths.AgentExe); err != nil {
		return fail("verify", err)
	}
	if !reused {
		m.progress("Enrolling with ReadyFleet…")
		if err := m.enrollFn(ctx, m.cfg, payload, m.onPendingApproval()); err != nil {
			return fail("enroll", err)
		}
		enrolled = true
	}
	m.progress("Securing local credentials…")
	if err := m.protectKeyFn(m.cfg.Paths.KeyPath); err != nil {
		return fail("credentials", err)
	}
	m.progress("Repairing the startup entry…")
	startupTouched = true
	if err := m.registerTaskFn(m.cfg.Paths.AgentExe); err != nil {
		return fail("startup", err)
	}
	if err := m.prepareEngineFn(); err != nil {
		m.progress("Engine preferences could not be updated; ask support if streaming needs attention.")
	}
	m.progress("Starting the agent…")

	if err := m.startAgentFn(m.cfg.Paths.AgentExe); err != nil {
		return fail("start", err)
	}
	m.progress("Confirming a new connection…")
	if err := m.waitForConnectedFn(ctx, m.cfg.Paths.LogPath, connectHealthcheckTimeout); err != nil {
		return fail("connect", err)
	}
	return nil
}
func removeIfPresent(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func tailLog(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > 16384 {
		_, _ = f.Seek(-16384, io.SeekEnd)
	}
	scanner := bufio.NewScanner(io.LimitReader(f, 16384))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return redactLines(lines)
}
func waitForFreshConnection(ctx context.Context, path string, timeout time.Duration, since time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, line := range tailRawLog(path) {
			var entry struct {
				Time time.Time `json:"time"`
				Msg  string    `json:"msg"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "agent connected" && !entry.Time.Before(since) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no new connection: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func tailRawLog(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > 65536 {
		_, _ = f.Seek(-65536, io.SeekEnd)
	}
	b, _ := io.ReadAll(io.LimitReader(f, 65536))
	return strings.Split(string(b), "\n")
}

func preflight(ctx context.Context, cfg Config) error {
	if err := platformPreflight(cfg.Paths.Root); err != nil {
		return err
	}
	endpoint, err := url.Parse(cfg.ServerURL)
	if err != nil || endpoint.Host == "" {
		return fmt.Errorf("invalid controller URL")
	}
	endpoint.Scheme = "https"
	endpoint.Path = "/enroll/v1/redeem"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return enroll.CheckConnectivity(ctx, endpoint.String())
}
