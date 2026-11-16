package solarbeam

// Size-capped capture of the SolarBeam child processes' stdout/stderr
// (bridge.log, and engine.log where the agent launches the engine itself).
//
// Why this exists: the agent used to hand each child an O_APPEND file as its
// stdout/stderr and never looked at it again. Nothing ever rotated or
// truncated it, so every session since install appended to the same file —
// Ports-Admin's C:\ProgramData\ReadyFleet\logs\bridge.log reached 97 MB (26 Sep
// 2026). The bridge itself only writes to stderr (Go's standard log package);
// the file, and so the fix, are the agent's.
//
// Design. A child that owns its log handle for the whole session cannot be
// rotated from outside on Windows: renaming a file another process holds open
// without FILE_SHARE_DELETE fails, and rotating only at launch would still
// let one long session grow without bound. So the child now gets a pipe
// (os/exec creates one whenever Stdout is not an *os.File) and the agent
// copies it into a rotatingLog, which:
//
//   - rotates by size: bridge.log -> bridge.log.1 -> ... -> bridge.log.5,
//     the oldest dropped. At most (childLogBackups+1) * childLogMaxBytes on
//     disk per log, i.e. 60 MB.
//   - checks the size when a child attaches, so an existing oversized file is
//     dealt with at the first session start after the upgrade (see rotate:
//     a file over the cap keeps only its last childLogMaxBytes, from a line
//     boundary, as .1 — the 97 MB file becomes a 10 MB .1 and a fresh log).
//   - never fails the child. Write always reports success; if the file
//     cannot be opened, rotated or written, the bytes are dropped rather than
//     returned as an error, because an error makes os/exec stop draining the
//     pipe and the child would then block (Windows) or take SIGPIPE (Linux)
//     on its next log line. Logging is diagnostic; the session is not.
//   - is shared per path within the process, reference counted by attached
//     children, and closes the file when the last one detaches — so a stale
//     child from the previous session still draining cannot interleave
//     rotations with the new one, and between sessions the agent holds no
//     handle on the file (it can be deleted or copied freely).
//
// Rename can still fail at rotation (e.g. a pre-upgrade bridge, launched with
// the old inherited handle, is still dying; or someone has the file open in an
// editor). rotate then falls back to copy-tail-then-truncate-in-place, and if
// even that leaves the file over the cap, writes are dropped and rotation is
// retried after childLogRetryInterval rather than on every write.

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	// childLogMaxBytes caps each file (current and every backup).
	childLogMaxBytes = 10 << 20
	// childLogBackups is how many rotated files are kept (name.1..name.N).
	childLogBackups = 5
	// childLogRetryInterval throttles reopen/rotate attempts after a failure
	// so a broken log directory costs one syscall burst per interval, not
	// one per line the child prints.
	childLogRetryInterval = 30 * time.Second
	// childLogTailAlign is how far into a trimmed tail rotate looks for a
	// newline to start the kept part on a whole line.
	childLogTailAlign = 64 << 10
	// childPipeWaitDelay bounds how long exec.Cmd.Wait keeps draining a
	// child's output pipe after the child has exited, so a stray grandchild
	// that inherited the pipe cannot pin the log attachment (and Wait) open.
	childPipeWaitDelay = 2 * time.Second
)

// Seams, so tests can force the failure paths without an OS that misbehaves.
var (
	childLogRename = os.Rename
	childLogNow    = time.Now
	// childLogTruncate empties the current file BY PATH. It must not go
	// through the append handle: on Windows, Go opens O_APPEND files with
	// FILE_APPEND_DATA but without FILE_WRITE_DATA, so (*os.File).Truncate
	// on that handle fails with "Access is denied" (seen on Ports-Admin,
	// 26 Sep 2026: a 97 MB bridge.log was never emptied and every retry
	// re-copied its tail into .1..5, i.e. 10 MB of writes per 30 s).
	childLogTruncate = os.Truncate
)

// rotatingLog is the shared, size-capped sink for one log path.
type rotatingLog struct {
	path     string
	dirPerm  fs.FileMode
	filePerm fs.FileMode
	maxBytes int64
	backups  int

	mu         sync.Mutex
	f          *os.File
	size       int64
	refs       int
	retryAfter time.Time
}

var childLogs = struct {
	mu   sync.Mutex
	logs map[string]*rotatingLog
}{logs: map[string]*rotatingLog{}}

// childLogHandle is one child's attachment to a shared rotatingLog. It is
// what the caller gives os/exec as Stdout/Stderr, and Close detaches it.
type childLogHandle struct {
	log  *rotatingLog
	once sync.Once
}

func (h *childLogHandle) Write(p []byte) (int, error) { return h.log.Write(p) }

// Close detaches this child; the file is closed when the last child has
// detached. Idempotent.
func (h *childLogHandle) Close() error {
	h.once.Do(h.log.detach)
	return nil
}

// openChildLog attaches a child to dir/name's shared rotating log, using the
// package's standard caps. It never fails: a directory or file that cannot be
// created yields a handle that drops writes (see rotatingLog.Write).
func openChildLog(dir, name string, dirPerm, filePerm fs.FileMode) io.WriteCloser {
	return attachRotatingLog(filepath.Join(dir, name), dirPerm, filePerm, childLogMaxBytes, childLogBackups)
}

func attachRotatingLog(path string, dirPerm, filePerm fs.FileMode, maxBytes int64, backups int) *childLogHandle {
	childLogs.mu.Lock()
	l, ok := childLogs.logs[path]
	if !ok {
		l = &rotatingLog{path: path, dirPerm: dirPerm, filePerm: filePerm, maxBytes: maxBytes, backups: backups}
		childLogs.logs[path] = l
	}
	childLogs.mu.Unlock()

	l.mu.Lock()
	l.refs++
	if l.f == nil {
		// Opening now (not lazily on the first write) is what trims an
		// oversized file left by an older agent at the first session start.
		l.retryAfter = time.Time{}
		l.openLocked()
	}
	l.mu.Unlock()
	return &childLogHandle{log: l}
}

func (l *rotatingLog) detach() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refs > 0 {
		l.refs--
	}
	if l.refs == 0 {
		l.closeLocked()
	}
}

// Write appends p, rotating first if p would take the file over the cap. It
// always returns len(p), nil — see the file comment.
func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil && !l.openLocked() {
		return len(p), nil
	}
	if l.size > 0 && l.size+int64(len(p)) > l.maxBytes {
		if !l.rotateLocked() {
			return len(p), nil
		}
	}
	if l.size >= l.maxBytes {
		// Rotation could not bring the file under the cap: drop, don't grow.
		return len(p), nil
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// openLocked opens the current file for append, rotating it first if it is
// already at or over the cap. It reports whether a usable file is open.
// Caller holds l.mu.
func (l *rotatingLog) openLocked() bool {
	if l.f != nil {
		return true
	}
	if now := childLogNow(); now.Before(l.retryAfter) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(l.path), l.dirPerm); err != nil {
		l.backOffLocked()
		return false
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, l.filePerm)
	if err != nil {
		l.backOffLocked()
		return false
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	l.f, l.size = f, size
	if l.size >= l.maxBytes {
		return l.rotateLocked()
	}
	return true
}

func (l *rotatingLog) closeLocked() {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	l.size = 0
}

func (l *rotatingLog) backOffLocked() {
	l.retryAfter = childLogNow().Add(childLogRetryInterval)
}

func (l *rotatingLog) backupName(i int) string { return l.path + "." + strconv.Itoa(i) }

// rotateLocked shifts the backups along, moves the current file to .1, and
// reopens an empty current file. It reports whether the reopened file is
// under the cap (i.e. whether writing may continue). Caller holds l.mu.
//
// The current file becomes .1 by rename when it is within the cap. When it is
// over the cap (an unrotated file from an older agent) or the rename fails,
// its last maxBytes are copied into .1 instead — starting at a line boundary
// where one is near — and the file is truncated in place. Every step is best
// effort; the only thing that matters afterwards is the final size check.
func (l *rotatingLog) rotateLocked() bool {
	size := l.size
	l.closeLocked()

	if l.backups > 0 {
		_ = os.Remove(l.backupName(l.backups))
		for i := l.backups - 1; i >= 1; i-- {
			// Missing backups are normal; a stuck one is left behind rather
			// than allowed to stop the rotation (os.Rename replaces an
			// existing destination on every platform we build for).
			_ = childLogRename(l.backupName(i), l.backupName(i+1))
		}
		renamed := false
		if size <= l.maxBytes {
			renamed = childLogRename(l.path, l.backupName(1)) == nil
		}
		if !renamed {
			_ = copyTail(l.path, l.backupName(1), l.maxBytes, l.filePerm)
		}
	}

	// The rename did not happen (or there are no backups): empty the file
	// in place, by path, before reopening it for append. os.Truncate opens
	// its own write handle, which works on Windows even while another
	// process holds the file open with FILE_SHARE_WRITE.
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > 0 {
		_ = childLogTruncate(l.path, 0)
	}

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, l.filePerm)
	if err != nil {
		l.backOffLocked()
		return false
	}
	l.f = f
	l.size = 0
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	if l.size >= l.maxBytes {
		l.closeLocked()
		l.backOffLocked()
		return false
	}
	return true
}

// copyTail writes the last maxBytes of src to dst (replacing dst), starting
// just after the first newline in the kept region if one is within
// childLogTailAlign bytes, so the backup does not open on half a line.
func copyTail(src, dst string, maxBytes int64, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	start := int64(0)
	if fi.Size() > maxBytes {
		start = fi.Size() - maxBytes
		probe := make([]byte, min(int64(childLogTailAlign), maxBytes))
		n, _ := in.ReadAt(probe, start)
		if idx := bytes.IndexByte(probe[:n], '\n'); idx >= 0 && int64(idx+1) < maxBytes {
			start += int64(idx + 1)
		}
	}
	if _, err := in.Seek(start, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, io.LimitReader(in, maxBytes))
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	return cerr
}
