package lightinstall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
)

func resilientManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := newTestManager()
	root := filepath.Join(t.TempDir(), "install")
	m.cfg = Config{ServerURL: "wss://fleet.example:8643/agent/v1", AgentBytes: []byte("new binary"), Paths: Paths{Root: root, AgentExe: filepath.Join(root, "agent"), ConfigPath: filepath.Join(root, "config.json"), KeyPath: filepath.Join(root, "client.key"), LogPath: filepath.Join(root, "logs", "agent.log")}}
	m.preflightFn = func(context.Context, Config) error { return nil }
	m.stopAgentFn = func(string) error { return nil }
	m.stageAgentFn = stageAgent
	m.removeAllFn = os.RemoveAll
	m.unlinkOnlineFn = func(context.Context, string) error { return nil }
	m.enrollFn = func(context.Context, Config, Payload, func()) error {
		return os.WriteFile(m.cfg.Paths.KeyPath, []byte("new credentials"), 0600)
	}
	return m
}
func TestResilientRollbackEveryFatalStep(t *testing.T) {
	for _, step := range []string{"stage", "verify", "enroll", "credentials", "startup", "start", "connect"} {
		t.Run(step, func(t *testing.T) {
			m := resilientManager(t)
			boom := errors.New("injected failure")
			removedStartup := false
			stopped := 0
			unlinked := false
			m.stopAgentFn = func(string) error { stopped++; return nil }
			m.removeTaskFn = func() error { removedStartup = true; return nil }
			m.unlinkOnlineFn = func(context.Context, string) error { unlinked = true; return nil }
			switch step {
			case "stage":
				m.stageAgentFn = func(string, []byte) error { return boom }
			case "verify":
				m.verifyAgentFn = func(string) error { return boom }
			case "enroll":
				m.enrollFn = func(context.Context, Config, Payload, func()) error {
					_ = os.WriteFile(m.cfg.Paths.KeyPath, []byte("partial"), 0600)
					return boom
				}
			case "credentials":
				m.protectKeyFn = func(string) error { return boom }
			case "startup":
				m.registerTaskFn = func(string) error { return boom }
			case "start":
				m.startAgentFn = func(string) error { return boom }
			case "connect":
				m.waitForConnectedFn = func(context.Context, string, time.Duration) error { return boom }
			}
			err := m.Install(context.Background(), Payload{ResilientInstall: true})
			var f *Failure
			if !errors.As(err, &f) || f.Step != step || !errors.Is(err, boom) {
				t.Fatalf("failure=%v", err)
			}
			if f.Rollback != nil {
				t.Fatalf("rollback=%v", f.Rollback)
			}
			if _, err = os.Stat(m.cfg.Paths.Root); !os.IsNotExist(err) {
				t.Fatalf("fresh install left files: %v", err)
			}
			if stopped != 2 {
				t.Fatalf("stop calls=%d", stopped)
			}
			wantStartup := step == "startup" || step == "start" || step == "connect"
			if removedStartup != wantStartup {
				t.Fatalf("startup removed=%v", removedStartup)
			}
			if unlinked != (step == "credentials" || wantStartup) {
				t.Fatalf("revoke new identity=%v", unlinked)
			}
		})
	}
}
func TestResilientPreflightAndStopFailureDoNotMutate(t *testing.T) {
	for _, step := range []string{"preflight", "stop"} {
		t.Run(step, func(t *testing.T) {
			m := resilientManager(t)
			_ = os.MkdirAll(m.cfg.Paths.Root, 0700)
			_ = os.WriteFile(m.cfg.Paths.AgentExe, []byte("old"), 0755)
			if step == "preflight" {
				m.preflightFn = func(context.Context, Config) error { return errors.New("disk full") }
			} else {
				m.stopAgentFn = func(string) error { return errors.New("cannot stop") }
			}
			m.stageAgentFn = func(string, []byte) error { t.Fatal("staged before preflight/stop passed"); return nil }
			if err := m.Install(context.Background(), Payload{ResilientInstall: true}); err == nil {
				t.Fatal("expected failure")
			}
			data, _ := os.ReadFile(m.cfg.Paths.AgentExe)
			if string(data) != "old" {
				t.Fatalf("old binary lost: %q", data)
			}
		})
	}
}
func TestResilientRestoresHalfInstallAndUnrelatedFiles(t *testing.T) {
	m := resilientManager(t)
	_ = os.MkdirAll(m.cfg.Paths.Root, 0700)
	for _, p := range []string{m.cfg.Paths.AgentExe, m.cfg.Paths.ConfigPath, m.cfg.Paths.KeyPath, filepath.Join(m.cfg.Paths.Root, "notes.txt")} {
		_ = os.WriteFile(p, []byte("previous:"+filepath.Base(p)), 0600)
	}
	m.registerTaskFn = func(string) error { return errors.New("startup denied") }
	if err := m.Install(context.Background(), Payload{ResilientInstall: true}); err == nil {
		t.Fatal("expected failure")
	}
	for _, p := range []string{m.cfg.Paths.AgentExe, m.cfg.Paths.ConfigPath, m.cfg.Paths.KeyPath, filepath.Join(m.cfg.Paths.Root, "notes.txt")} {
		b, _ := os.ReadFile(p)
		if string(b) != "previous:"+filepath.Base(p) {
			t.Errorf("didn't restore %s", filepath.Base(p))
		}
	}
}
func validIdentity(t *testing.T, m *Manager) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	client := &x509.Certificate{Subject: pkix.Name{CommonName: "existing"}, SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	cert, err := x509.CreateCertificate(rand.Reader, client, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	rawKey, _ := x509.MarshalECPrivateKey(key)
	_ = os.MkdirAll(m.cfg.Paths.Root, 0700)
	for name, data := range map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), "client.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), "client.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: rawKey})} {
		if err = os.WriteFile(filepath.Join(m.cfg.Paths.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Config{ServerURL: m.cfg.ServerURL, MachineID: "existing", OwnerActorID: "owner", KeyPath: m.cfg.Paths.KeyPath, CertPath: filepath.Join(m.cfg.Paths.Root, "client.crt"), CAPath: filepath.Join(m.cfg.Paths.Root, "ca.crt")}
	if err = config.Save(m.cfg.Paths.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(m.cfg.Paths.AgentExe, []byte("old"), 0755)
}
func TestResilientRerunReusesIdentityWithoutSpendingToken(t *testing.T) {
	m := resilientManager(t)
	validIdentity(t, m)
	before, _ := os.ReadFile(m.cfg.Paths.KeyPath)
	m.enrollFn = func(context.Context, Config, Payload, func()) error { t.Fatal("spent token on repair"); return nil }
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.Install(context.Background(), Payload{ResilientInstall: true, ExpectedOwnerActorID: "owner", EnrollmentToken: "already_spent"}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(m.cfg.Paths.KeyPath)
	if string(after) != string(before) {
		t.Fatal("identity changed")
	}
}
func TestRepairRollbackRestoresExistingIdentityAndRestarts(t *testing.T) {
	m := resilientManager(t)
	validIdentity(t, m)
	starts := 0
	m.enrollFn = func(context.Context, Config, Payload, func()) error { t.Fatal("must reuse"); return nil }
	m.startAgentFn = func(string) error { starts++; return nil }
	m.waitForConnectedFn = func(context.Context, string, time.Duration) error { return errors.New("offline") }
	m.unlinkOnlineFn = func(context.Context, string) error { t.Fatal("revoked old identity"); return nil }
	m.removeTaskFn = func() error { t.Fatal("removed existing startup"); return nil }
	if err := m.Install(context.Background(), Payload{ResilientInstall: true, ExpectedOwnerActorID: "owner"}); err == nil {
		t.Fatal("expected failure")
	}
	b, _ := os.ReadFile(m.cfg.Paths.AgentExe)
	if string(b) != "old" || starts != 2 || !reusableIdentity(m.cfg) {
		t.Fatal("repair did not restore old running installation")
	}
}
func TestRollbackFailureIsVisible(t *testing.T) {
	m := resilientManager(t)
	m.startAgentFn = func(string) error { return errors.New("start failed") }
	m.removeTaskFn = func() error { return errors.New("startup cannot be removed") }
	err := m.Install(context.Background(), Payload{ResilientInstall: true})
	var f *Failure
	if !errors.As(err, &f) || f.Rollback == nil || !strings.Contains(f.Instruction(), "Cleanup could not finish") {
		t.Fatal(err)
	}
}
func TestFreshHealthcheckRejectsOldConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	now := time.Now()
	old := fmt.Sprintf("{\"time\":%q,\"msg\":\"agent connected\"}\n", now.Add(-time.Hour).Format(time.RFC3339Nano))
	_ = os.WriteFile(path, []byte(old), 0600)
	if err := waitForFreshConnection(context.Background(), path, 10*time.Millisecond, now); err == nil {
		t.Fatal("accepted stale connection")
	}
	fresh := fmt.Sprintf("{\"time\":%q,\"msg\":\"agent connected\"}\n", now.Add(time.Second).Format(time.RFC3339Nano))
	_ = os.WriteFile(path, []byte(old+fresh), 0600)
	if err := waitForFreshConnection(context.Background(), path, time.Second, now); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSetupFailsBeforeStaging(t *testing.T) {
	m := resilientManager(t)
	release, err := acquireInstallLock(m.cfg.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m.stageAgentFn = func(string, []byte) error { t.Fatal("concurrent setup staged"); return nil }
	err = m.Install(context.Background(), Payload{ResilientInstall: true})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "setup_running" {
		t.Fatal(err)
	}
}
func TestRepairRefusesDifferentOwnerWithoutChangingFiles(t *testing.T) {
	m := resilientManager(t)
	validIdentity(t, m)
	m.enrollFn = func(context.Context, Config, Payload, func()) error { t.Fatal("spent token"); return nil }
	err := m.Install(context.Background(), Payload{ResilientInstall: true, ExpectedOwnerActorID: "different"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "owner_mismatch" {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.cfg.Paths.AgentExe)
	if string(data) != "old" {
		t.Fatal("changed other owner's installation")
	}
}
func TestRollbackRestoresStartupSnapshotAndProtectsOldKey(t *testing.T) {
	m := resilientManager(t)
	_ = os.MkdirAll(m.cfg.Paths.Root, 0700)
	_ = os.WriteFile(m.cfg.Paths.KeyPath, []byte("old key"), 0600)
	restored := false
	protected := 0
	m.snapshotStartupFn = func() (func() error, error) { return func() error { restored = true; return nil }, nil }
	m.protectKeyFn = func(string) error { protected++; return nil }
	m.startAgentFn = func(string) error { return errors.New("start failed") }
	if err := m.Install(context.Background(), Payload{ResilientInstall: true}); err == nil {
		t.Fatal("expected fail")
	}
	if !restored || protected != 2 {
		t.Fatalf("startup restored=%v protect calls=%d", restored, protected)
	}
}
