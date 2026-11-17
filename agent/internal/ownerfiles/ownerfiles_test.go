package ownerfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func fixture(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := New(dir, "owner", "controller.test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, dir
}

func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

func TestDisabledAndInvalidConfiguration(t *testing.T) {
	m, err := New("", "owner", "host")
	if err != nil || m != nil {
		t.Fatal("empty root must disable")
	}
	for _, c := range [][3]string{{"relative", "owner", "host"}, {t.TempDir(), "", "host"}, {t.TempDir(), "owner", ""}, {string(filepath.Separator), "owner", "host"}} {
		if _, err := New(c[0], c[1], c[2]); err == nil {
			t.Fatalf("accepted %v", c)
		}
	}
}

func TestOwnerAndRateBoundary(t *testing.T) {
	m, _ := fixture(t)
	for _, owner := range []string{"", "other", "OWNER"} {
		if _, err := m.begin(owner); err == nil {
			t.Fatalf("accepted %q", owner)
		}
	}
	done, err := m.begin("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.begin("owner"); err == nil {
		t.Fatal("concurrent transfer admitted")
	}
	done()
	for i := 1; i < 10; i++ {
		done, err = m.begin("owner")
		if err != nil {
			t.Fatal(err)
		}
		done()
	}
	if _, err := m.begin("owner"); err == nil {
		t.Fatal("rate cap not enforced")
	}
}

func TestPathBoundary(t *testing.T) {
	m, dir := fixture(t)
	for _, p := range []string{"../escape", filepath.Join(dir, "..", "escape"), "sub/../../escape", ".ssh/key", "sub/.config", "file:stream", "bad\x00name"} {
		if _, err := m.relative(p, false); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	for _, p := range []string{"run.exe", "startup.LNK", "run.ps1", "auto.desktop", "x.sh"} {
		if _, err := m.relative(p, true); err == nil {
			t.Fatalf("accepted executable %q", p)
		}
	}
	for _, p := range []string{"notes.txt", filepath.Join(dir, "notes.txt"), "sub/report.pdf"} {
		if _, err := m.relative(p, true); err != nil {
			t.Fatalf("refused %q: %v", p, err)
		}
	}
}

func TestSymlinkEscapeAndRootListing(t *testing.T) {
	m, dir := fixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := m.list("escape"); err == nil {
		t.Fatal("symlink escaped")
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := m.list("")
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != dir || out.Parent != "" || len(out.Entries) != 1 || out.Entries[0].Name != "visible.txt" {
		t.Fatalf("root leaks: %+v", out)
	}
	if _, err = m.upload(context.Background(), proto.UploadFileArgs{TargetPath: "escape/new.txt", DownloadURL: "https://controller.test/bytes", SHA256: hash(nil), DryRun: true}); err == nil {
		t.Fatal("upload dry run escaped root")
	}
}

func TestURLBoundary(t *testing.T) {
	m, _ := fixture(t)
	for _, u := range []string{"http://controller.test/file", "https://evil.test/file", "https://owner:secret@controller.test/file", "https://controller.test/file?q=token", "https://controller.test/file#fragment", "https://controller.test"} {
		if err := m.endpoint(u); err == nil {
			t.Fatalf("accepted %q", u)
		}
	}
	if err := m.endpoint("https://controller.test/file/token"); err != nil {
		t.Fatal(err)
	}
}

func TestUploadVerifiedDryRunHashNoOverwriteAndSize(t *testing.T) {
	m, dir := fixture(t)
	body := []byte("a small document")
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write(body) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	m.host = u.Hostname()
	m.client = srv.Client()
	m.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a := proto.UploadFileArgs{DownloadURL: srv.URL + "/file", TargetPath: filepath.Join(dir, "report.txt"), SHA256: hash(body), DryRun: true}
	if _, err := m.upload(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("dry run fetched bytes")
	}
	if _, err := os.Stat(a.TargetPath); !os.IsNotExist(err) {
		t.Fatal("dry run wrote file")
	}
	a.DryRun = false
	a.SHA256 = hash([]byte("wrong"))
	if _, err := m.upload(context.Background(), a); err == nil {
		t.Fatal("bad hash accepted")
	}
	if _, err := os.Stat(a.TargetPath); !os.IsNotExist(err) {
		t.Fatal("bad hash wrote file")
	}
	a.SHA256 = hash(body)
	if _, err := m.upload(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.TargetPath)
	if err != nil || string(b) != string(body) {
		t.Fatalf("wrong saved bytes: %q %v", b, err)
	}
	before := requests
	if _, err := m.upload(context.Background(), a); err == nil {
		t.Fatal("overwrite accepted")
	}
	if requests != before {
		t.Fatal("existing target fetched")
	}
	body = []byte(strings.Repeat("x", int(MaxBytes)+1))
	a.TargetPath = "large.txt"
	a.SHA256 = hash(body)
	if _, err := m.upload(context.Background(), a); err == nil {
		t.Fatal("over-cap upload accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "large.txt")); !os.IsNotExist(err) {
		t.Fatal("over-cap upload wrote file")
	}
}

func TestDownloadReceiptDryRunAndOversize(t *testing.T) {
	m, dir := fixture(t)
	body := []byte("document")
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), body, 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "PUT" || r.Header.Get("X-Fleet-File-SHA256") != hash(body) {
			t.Errorf("wrong transfer request")
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	m.host = u.Hostname()
	m.client = srv.Client()
	a := proto.DownloadFileArgs{Path: "doc.txt", UploadURL: srv.URL + "/receive", DryRun: true}
	if _, err := m.download(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("dry run sent bytes")
	}
	a.DryRun = false
	if _, err := m.download(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("missing transfer")
	}
	f, err := os.Create(filepath.Join(dir, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(MaxBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	a.Path = "large.txt"
	if _, err := m.download(context.Background(), a); err == nil {
		t.Fatal("oversize read accepted")
	}
}

func TestRedirectNeverReceivesFileOrFetch(t *testing.T) {
	m, dir := fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	received := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
			return
		}
		received++
		w.Write([]byte("private"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	m.host = u.Hostname()
	m.client.Transport = srv.Client().Transport
	if _, err := m.download(context.Background(), proto.DownloadFileArgs{Path: "doc.txt", UploadURL: srv.URL + "/redirect"}); err == nil {
		t.Fatal("download redirect accepted")
	}
	if _, err := m.upload(context.Background(), proto.UploadFileArgs{TargetPath: "new.txt", DownloadURL: srv.URL + "/redirect", SHA256: hash([]byte("private"))}); err == nil {
		t.Fatal("upload redirect accepted")
	}
	if received != 0 {
		t.Fatal("followed redirect")
	}
}

func TestHandlersStrictArgsAndOwner(t *testing.T) {
	m, _ := fixture(t)
	for _, args := range []string{`{"requester_id":"other"}`, `{"requester_id":"owner","script":"whoami"}`, `null`} {
		env, err := proto.NewEnvelope(proto.MsgCommand, uuid.New(), nil, proto.CommandData{Kind: proto.CmdListDir, Args: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		var result proto.ResultData
		m.RunListDir(context.Background(), env, func(ctx context.Context, e proto.Envelope) error {
			if e.Type == proto.MsgResult {
				return e.DecodeData(&result)
			}
			return nil
		})
		if result.ExitCode == 0 {
			t.Fatalf("accepted %s", args)
		}
	}
}
