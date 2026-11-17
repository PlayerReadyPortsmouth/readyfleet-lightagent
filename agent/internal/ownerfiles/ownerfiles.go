// Package ownerfiles implements the light agent's optional owner-only file
// exchange. The root comes exclusively from local configuration; no command
// can choose a new root, overwrite a file, execute one, or escape os.Root.
package ownerfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const MaxBytes int64 = 8 << 20
const Timeout = 2 * time.Minute

type Manager struct {
	root                *os.Root
	owner, host, folder string
	client              *http.Client
	mu                  sync.Mutex
	window              time.Time
	count               int
	busy                bool
}

// New opens an existing user-selected folder. Empty configuration disables
// the capability. A folder handle, rather than a resolved path prefix, keeps
// later symlink replacement from escaping the selected tree.
func New(folder, owner, host string) (*Manager, error) {
	if folder == "" {
		return nil, nil
	}
	if !filepath.IsAbs(folder) || owner == "" || host == "" || filepath.Dir(filepath.Clean(folder)) == filepath.Clean(folder) {
		return nil, errors.New("owner files require a selected absolute folder, enrolled owner and controller host")
	}
	r, err := os.OpenRoot(filepath.Clean(folder))
	if err != nil {
		return nil, fmt.Errorf("open selected owner folder: %w", err)
	}
	return &Manager{root: r, owner: owner, host: host, folder: filepath.Clean(folder), client: &http.Client{
		Timeout: Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("owner file redirects refused") },
	}}, nil
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	return m.root.Close()
}

func (m *Manager) begin(owner string) (func(), error) {
	if m == nil || m.root == nil || owner == "" || owner != m.owner {
		return nil, errors.New("owner file access denied")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.window) >= time.Minute {
		m.window = time.Now()
		m.count = 0
	}
	if m.count >= 10 || m.busy {
		return nil, errors.New("owner file rate limit or transfer busy")
	}
	m.count++
	m.busy = true
	return func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }, nil
}

func (m *Manager) relative(path string, write bool) (string, error) {
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(m.folder, path)
		if err != nil {
			return "", errors.New("path outside selected folder")
		}
	}
	if path != "." && !filepath.IsLocal(path) {
		return "", errors.New("path outside selected folder")
	}
	if strings.ContainsAny(path, "\x00:") {
		return "", errors.New("invalid owner file path")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || (strings.HasPrefix(part, ".") && part != ".") {
			return "", errors.New("hidden paths and traversal refused")
		}
	}
	path = filepath.Clean(path)
	if write {
		if path == "." {
			return "", errors.New("a destination filename is required")
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".exe", ".dll", ".bat", ".cmd", ".ps1", ".lnk", ".desktop", ".url", ".sh", ".command", ".app", ".vbs", ".js", ".py", ".reg", ".service", ".plist", ".msc", ".hta", ".scr", ".com":
			return "", errors.New("executable and startup files refused")
		}
	}
	return path, nil
}

func (m *Manager) endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), m.host) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return errors.New("transfer URL must be HTTPS on the controller host without credentials, query or fragment")
	}
	return nil
}

func decode[T any](env proto.Envelope, want proto.CommandKind, args *T) error {
	var cmd proto.CommandData
	if err := env.DecodeData(&cmd); err != nil {
		return err
	}
	if cmd.Kind != want {
		return errors.New("unexpected owner file command")
	}
	d := json.NewDecoder(bytes.NewReader(cmd.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(args); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing owner file arguments")
	}
	return nil
}

func reply(ctx context.Context, env proto.Envelope, send transport.SendFunc, out any, err error, start time.Time) {
	result := proto.ResultData{Duration: time.Since(start)}
	if err == nil {
		b, e := json.Marshal(out)
		if e == nil {
			var output proto.Envelope
			output, e = proto.NewEnvelope(proto.MsgOutput, uuid.Nil, &env.ID, proto.OutputData{Stream: proto.StreamStdout, Chunk: string(b)})
			if e == nil {
				e = send(ctx, output)
			}
		}
		err = e
	}
	if err != nil {
		result.ExitCode = -1
		result.Error = "owner files: " + err.Error()
	}
	final, e := proto.NewEnvelope(proto.MsgResult, uuid.Nil, &env.ID, result)
	if e == nil {
		_ = send(ctx, final)
	}
}

func (m *Manager) RunListDir(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
	start := time.Now()
	var args proto.ListDirArgs
	err := decode(env, proto.CmdListDir, &args)
	var out proto.ListDirResult
	if err == nil {
		var done func()
		done, err = m.begin(args.RequesterID)
		if err == nil {
			defer done()
			out, err = m.list(args.Path)
		}
	}
	reply(ctx, env, send, out, err, start)
}

func (m *Manager) list(path string) (proto.ListDirResult, error) {
	rel, err := m.relative(path, false)
	if err != nil {
		return proto.ListDirResult{}, err
	}
	f, err := openRead(m.root, rel)
	if err != nil {
		return proto.ListDirResult{}, err
	}
	defer f.Close()
	entries, err := f.ReadDir(proto.MaxListDirEntries + 1)
	if err != nil && err != io.EOF {
		return proto.ListDirResult{}, err
	}
	out := proto.ListDirResult{Path: filepath.Join(m.folder, rel), Entries: []proto.DirEntry{}}
	if rel != "." {
		out.Parent = filepath.Join(m.folder, filepath.Dir(rel))
	}
	if len(entries) > proto.MaxListDirEntries {
		out.Truncated = true
		entries = entries[:proto.MaxListDirEntries]
	}
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") || de.Type()&os.ModeSymlink != 0 {
			continue
		}
		fi, e := de.Info()
		if e != nil || (!fi.Mode().IsRegular() && !fi.IsDir()) {
			continue
		}
		out.Entries = append(out.Entries, proto.DirEntry{Name: de.Name(), IsDir: de.IsDir(), Size: fi.Size(), ModifiedAt: fi.ModTime().UTC()})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

func (m *Manager) RunDownloadFile(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
	start := time.Now()
	var args proto.DownloadFileArgs
	err := decode(env, proto.CmdDownloadFile, &args)
	var out any
	if err == nil {
		var done func()
		done, err = m.begin(args.RequesterID)
		if err == nil {
			defer done()
			ctx, cancel := context.WithTimeout(ctx, Timeout)
			defer cancel()
			out, err = m.download(ctx, args)
		}
	}
	reply(ctx, env, send, out, err, start)
}

func (m *Manager) download(ctx context.Context, args proto.DownloadFileArgs) (any, error) {
	rel, err := m.relative(args.Path, false)
	if err != nil {
		return nil, err
	}
	if err = m.endpoint(args.UploadURL); err != nil {
		return nil, err
	}
	f, err := openRead(m.root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > MaxBytes {
		return nil, errors.New("only regular files up to 8 MiB may be copied")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > MaxBytes {
		return nil, errors.New("file exceeds 8 MiB")
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	out := struct {
		proto.DownloadFileResult
		Verified bool `json:"verified"`
		DryRun   bool `json:"dry_run"`
	}{proto.DownloadFileResult{Path: filepath.Join(m.folder, rel), Size: int64(len(b)), SHA256: hash}, false, args.DryRun}
	if args.DryRun {
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, args.UploadURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Fleet-File-SHA256", hash)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, errors.New("transfer request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("transfer refused with status %d", resp.StatusCode)
	}
	out.Verified = true
	return out, nil
}

func (m *Manager) RunUploadFile(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
	start := time.Now()
	var args proto.UploadFileArgs
	err := decode(env, proto.CmdUploadFile, &args)
	var out any
	if err == nil {
		var done func()
		done, err = m.begin(args.RequesterID)
		if err == nil {
			defer done()
			ctx, cancel := context.WithTimeout(ctx, Timeout)
			defer cancel()
			out, err = m.upload(ctx, args)
		}
	}
	reply(ctx, env, send, out, err, start)
}

func (m *Manager) upload(ctx context.Context, args proto.UploadFileArgs) (any, error) {
	rel, err := m.relative(args.TargetPath, true)
	if err != nil {
		return nil, err
	}
	if err = m.endpoint(args.DownloadURL); err != nil {
		return nil, err
	}
	hash, err := hex.DecodeString(args.SHA256)
	if err != nil || len(hash) != sha256.Size || strings.ToLower(args.SHA256) != args.SHA256 {
		return nil, errors.New("invalid SHA256")
	}
	if _, err = m.root.Lstat(rel); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("destination exists or cannot be checked")
	}
	// Resolve/open the parent through os.Root before network access, including
	// dry-run. Parent creation and recursive copying are deliberately absent.
	parent, err := m.root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	out := struct {
		proto.UploadFileResult
		Verified bool `json:"verified"`
		DryRun   bool `json:"dry_run"`
	}{proto.UploadFileResult{TargetPath: filepath.Join(m.folder, rel), SHA256: args.SHA256}, false, args.DryRun}
	if args.DryRun {
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, errors.New("transfer request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transfer refused with status %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxBytes {
		return nil, errors.New("file exceeds 8 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, errors.New("transfer body failed")
	}
	if int64(len(b)) > MaxBytes {
		return nil, errors.New("file exceeds 8 MiB")
	}
	sum := sha256.Sum256(b)
	if !bytes.Equal(hash, sum[:]) {
		return nil, errors.New("SHA256 mismatch")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	name := filepath.Base(rel)
	f, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	n, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || n != len(b) {
		_ = parent.Remove(name)
		return nil, errors.New("destination write failed")
	}
	out.Size = int64(n)
	out.Verified = true
	return out, nil
}
