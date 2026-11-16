package solarbeam

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These drive rotatingLog through its writer interface only — the same thing
// os/exec's pipe-drain goroutine does — so no child process is spawned.

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

func TestRotatingLog_AppendsUnderCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.log")
	if err := os.WriteFile(path, []byte("earlier session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := attachRotatingLog(path, 0o755, 0o644, 100, 2)
	defer h.Close()
	if n, err := h.Write([]byte("this session\n")); n != 13 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "earlier session\nthis session\n" {
		t.Fatalf("content = %q; a log under the cap must be appended to, not rotated", got)
	}
	mustNotExist(t, path+".1")
}

func TestRotatingLog_RotatesBySizeAndKeepsBackupsBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	const maxBytes, backups = 100, 3
	h := attachRotatingLog(path, 0o755, 0o644, maxBytes, backups)
	defer h.Close()

	line := []byte("0123456789012345678\n") // 20 bytes
	for i := 0; i < 100; i++ {              // 2000 bytes: 20 files' worth
		if n, err := h.Write(line); n != len(line) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	var total int64
	for _, name := range []string{"bridge.log", "bridge.log.1", "bridge.log.2", "bridge.log.3"} {
		sz := fileSize(t, filepath.Join(dir, name))
		if sz > maxBytes {
			t.Fatalf("%s is %d bytes, over the %d cap", name, sz, maxBytes)
		}
		total += sz
	}
	mustNotExist(t, path+".4")
	if total > (backups+1)*maxBytes {
		t.Fatalf("total %d bytes exceeds (backups+1)*cap", total)
	}
	// Rotation happens on write boundaries, never mid-line.
	b1, _ := os.ReadFile(path + ".1")
	if !bytes.HasSuffix(b1, []byte("\n")) || !bytes.HasPrefix(b1, []byte("0123")) {
		t.Fatalf("backup split a line: %q", b1)
	}
}

// The Ports-Admin case: an unrotated bridge.log left by an older agent, far
// over the cap. Attaching (i.e. the first session start after the upgrade)
// keeps only its tail, from a whole line, and starts a fresh log — before the
// child has written anything.
func TestRotatingLog_FirstRunTrimsOversizedFileToItsTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	var old strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&old, "old line %04d\n", i) // 14 bytes each, 7000 bytes
	}
	if err := os.WriteFile(path, []byte(old.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("previous backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const maxBytes = 100
	h := attachRotatingLog(path, 0o755, 0o644, maxBytes, 5)
	defer h.Close()

	if sz := fileSize(t, path); sz != 0 {
		t.Fatalf("current log is %d bytes after first-run trim, want 0", sz)
	}
	tail, _ := os.ReadFile(path + ".1")
	if int64(len(tail)) > maxBytes {
		t.Fatalf("trimmed backup is %d bytes, over the cap", len(tail))
	}
	if !strings.HasPrefix(string(tail), "old line ") || !strings.HasSuffix(string(tail), "old line 0499\n") {
		t.Fatalf("trimmed backup is not the whole-line tail: %q", tail)
	}
	if prev, _ := os.ReadFile(path + ".2"); string(prev) != "previous backup\n" {
		t.Fatalf("existing backup not shifted to .2: %q", prev)
	}
	if _, err := h.Write([]byte("new session\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new session\n" {
		t.Fatalf("current log = %q", got)
	}
}

// Windows refuses to rename a file another process holds open without
// FILE_SHARE_DELETE (a pre-upgrade bridge still dying, an editor). Rotation
// must fall back to copying the tail and truncating in place, not give up and
// let the file grow.
func TestRotatingLog_RenameFailureFallsBackToCopyTruncate(t *testing.T) {
	orig := childLogRename
	t.Cleanup(func() { childLogRename = orig })
	childLogRename = func(string, string) error { return errors.New("sharing violation") }

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	h := attachRotatingLog(path, 0o755, 0o644, 50, 2)
	defer h.Close()

	for i := 0; i < 10; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("line %02d\n", i))) // 8 bytes
	}
	if sz := fileSize(t, path); sz > 50 {
		t.Fatalf("current log grew to %d bytes despite rename failing", sz)
	}
	b1, err := os.ReadFile(path + ".1")
	if err != nil || len(b1) == 0 || len(b1) > 50 {
		t.Fatalf("copy-truncate fallback did not produce a bounded .1: %q, %v", b1, err)
	}
}

// A log that cannot be opened at all must never surface as an error to the
// writer: os/exec stops draining the pipe on a write error, and the child
// would then block or die on its next log line.
func TestRotatingLog_UnwritableDirectoryDropsSilently(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := openChildLog(filepath.Join(blocker, "logs"), "bridge.log", 0o755, 0o644)
	defer h.Close()
	for i := 0; i < 3; i++ {
		if n, err := h.Write([]byte("hello\n")); n != 6 || err != nil {
			t.Fatalf("Write = %d, %v; want 6, nil", n, err)
		}
	}
}

func TestRotatingLog_RetriesOpenAfterBackoff(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	origNow := childLogNow
	t.Cleanup(func() { childLogNow = origNow })
	childLogNow = func() time.Time { return now }

	root := t.TempDir()
	blocker := filepath.Join(root, "logs")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := attachRotatingLog(filepath.Join(blocker, "bridge.log"), 0o755, 0o644, 100, 2)
	defer h.Close()
	_, _ = h.Write([]byte("dropped\n"))

	// The directory becomes creatable, but the retry waits out the backoff.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	_, _ = h.Write([]byte("still dropped\n"))
	mustNotExist(t, filepath.Join(blocker, "bridge.log"))

	now = now.Add(childLogRetryInterval + time.Second)
	_, _ = h.Write([]byte("kept\n"))
	if got, _ := os.ReadFile(filepath.Join(blocker, "bridge.log")); string(got) != "kept\n" {
		t.Fatalf("after backoff, log = %q", got)
	}
}

// Two children on the same path (a stale bridge still draining while the new
// one starts) share one sink, and the agent holds the file only while at
// least one is attached.
func TestRotatingLog_SharedPerPathAndClosedWhenLastDetaches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.log")
	a := attachRotatingLog(path, 0o755, 0o644, 100, 2)
	b := attachRotatingLog(path, 0o755, 0o644, 100, 2)
	if a.log != b.log {
		t.Fatal("two attachments to one path got different sinks")
	}
	_, _ = a.Write([]byte("a\n"))
	_, _ = b.Write([]byte("b\n"))
	_ = a.Close()
	_ = a.Close() // idempotent: must not detach b
	a.log.mu.Lock()
	open, refs := a.log.f != nil, a.log.refs
	a.log.mu.Unlock()
	if !open || refs != 1 {
		t.Fatalf("after one detach: open=%v refs=%d, want open, 1", open, refs)
	}
	_ = b.Close()
	a.log.mu.Lock()
	open = a.log.f != nil
	a.log.mu.Unlock()
	if open {
		t.Fatal("file still held open after the last child detached")
	}
	if got, _ := os.ReadFile(path); string(got) != "a\nb\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestCopyTail_NoNewlineKeepsRawTail(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyTail(src, dst, 64, 0o644); err != nil {
		t.Fatal(err)
	}
	if sz := fileSize(t, dst); sz != 64 {
		t.Fatalf("tail is %d bytes, want 64", sz)
	}
}

// Rotation must empty the current file by path, never through the append
// handle: on Windows an O_APPEND handle lacks FILE_WRITE_DATA, so
// (*os.File).Truncate on it is "Access is denied" and the oversized file was
// re-copied into the backups every 30 s instead of being emptied.
func TestRotatingLog_OversizedFileTruncatedByPath(t *testing.T) {
	origTrunc := childLogTruncate
	t.Cleanup(func() { childLogTruncate = origTrunc })
	var calls []string
	childLogTruncate = func(name string, size int64) error {
		calls = append(calls, name)
		return os.Truncate(name, size)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("old line\n"), 20), 0o644); err != nil { // 180 B > cap
		t.Fatal(err)
	}
	h := attachRotatingLog(path, 0o755, 0o644, 50, 2)
	defer h.Close()
	_, _ = h.Write([]byte("new line\n"))

	if len(calls) != 1 || calls[0] != path {
		t.Fatalf("truncate calls = %v; want exactly one, by path %q", calls, path)
	}
	if got, _ := os.ReadFile(path); string(got) != "new line\n" {
		t.Fatalf("current log = %q; want only the new line", got)
	}
	if sz := fileSize(t, path+".1"); sz == 0 || sz > 50 {
		t.Fatalf(".1 size = %d; want the bounded tail of the old file", sz)
	}
}

// If the file cannot be emptied at all, writes are dropped and rotation backs
// off rather than growing the file or re-copying its tail on every line.
func TestRotatingLog_TruncateFailureDropsAndBacksOff(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	origNow, origTrunc := childLogNow, childLogTruncate
	t.Cleanup(func() { childLogNow, childLogTruncate = origNow, origTrunc })
	childLogNow = func() time.Time { return now }
	calls := 0
	childLogTruncate = func(string, int64) error { calls++; return errors.New("access is denied") }

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("old line\n"), 20), 0o644); err != nil {
		t.Fatal(err)
	}
	h := attachRotatingLog(path, 0o755, 0o644, 50, 2)
	defer h.Close()
	for i := 0; i < 5; i++ {
		_, _ = h.Write([]byte("new line\n"))
	}
	if calls != 1 {
		t.Fatalf("truncate attempted %d times inside one backoff window; want 1", calls)
	}
	if sz := fileSize(t, path); sz != 180 {
		t.Fatalf("current log size = %d; want unchanged 180 (writes dropped)", sz)
	}
	now = now.Add(31 * time.Second)
	_, _ = h.Write([]byte("new line\n"))
	if calls != 2 {
		t.Fatalf("truncate not retried after backoff: %d calls", calls)
	}
}
