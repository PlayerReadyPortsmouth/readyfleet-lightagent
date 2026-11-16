//go:build linux

package solarbeam

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var linuxLinkSecretMu sync.Mutex

func linuxLinkRuntimeDir() string {
	if path := os.Getenv("XDG_RUNTIME_DIR"); path != "" {
		return path
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Geteuid()))
}

func openLinkDirectory(path string, private bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, errLinkCredential
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, errLinkCredential
	}
	fail := func() (int, error) { unix.Close(fd); return -1, errLinkCredential }
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			next, err := unix.Openat(fd, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return fail()
			}
			unix.Close(fd)
			fd = next
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || st.Mode&0o022 != 0 {
			return fail()
		}
		if private && i == len(parts)-1 && (st.Uid != uint32(os.Geteuid()) || st.Mode&0o7777 != 0o700) {
			return fail()
		}
	}
	return fd, nil
}

func validLinkFile(fd int, name string) (*unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o7777 != 0o600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return nil, errLinkCredential
	}
	return &st, nil
}

func writeLinuxLinkSecret(runtimeDir string, secret []byte) (string, func() error, error) {
	linuxLinkSecretMu.Lock()
	defer linuxLinkSecretMu.Unlock()
	if len(secret) != 64 || strings.IndexFunc(string(secret), func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) >= 0 {
		return "", nil, errLinkCredential
	}
	runtimeFD, err := openLinkDirectory(runtimeDir, true)
	if err != nil {
		return "", nil, err
	}
	defer unix.Close(runtimeFD)
	const name = "solarbeam-link-auth"
	if err := unix.Mkdirat(runtimeFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return "", nil, errLinkCredential
	}
	fd, err := unix.Openat(runtimeFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", nil, errLinkCredential
	}
	defer unix.Close(fd)
	var parent unix.Stat_t
	if unix.Fstat(fd, &parent) != nil || parent.Uid != uint32(os.Geteuid()) || parent.Mode&0o7777 != 0o700 {
		return "", nil, errLinkCredential
	}
	if _, err := validLinkFile(fd, "secret"); err != nil {
		return "", nil, err
	}
	tmp := "secret-" + rand.Text()
	secretFD, err := unix.Openat(fd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", nil, errLinkCredential
	}
	defer unix.Unlinkat(fd, tmp, 0)
	file := os.NewFile(uintptr(secretFD), "link credential")
	defer file.Close()
	// An unusually restrictive umask must not silently produce an unreadable key.
	if unix.Fchmod(secretFD, 0o600) != nil {
		return "", nil, errLinkCredential
	}
	if _, err := file.Write(secret); err != nil {
		return "", nil, errLinkCredential
	}
	if file.Sync() != nil {
		return "", nil, errLinkCredential
	}
	var generation unix.Stat_t
	if unix.Fstat(secretFD, &generation) != nil {
		return "", nil, errLinkCredential
	}
	if unix.Renameat(fd, tmp, fd, "secret") != nil {
		return "", nil, errLinkCredential
	}
	path := filepath.Join(runtimeDir, name, "secret")
	cleanup := func() error {
		linuxLinkSecretMu.Lock()
		defer linuxLinkSecretMu.Unlock()
		currentFD, err := openLinkDirectory(filepath.Dir(path), true)
		if err != nil {
			return err
		}
		defer unix.Close(currentFD)
		current, err := validLinkFile(currentFD, "secret")
		if err != nil {
			return err
		}
		if current == nil || current.Dev != generation.Dev || current.Ino != generation.Ino {
			return nil
		}
		if unix.Unlinkat(currentFD, "secret", 0) != nil {
			return errLinkCredential
		}
		return nil
	}
	return path, cleanup, nil
}

func (m *Manager) provisionPlatformLinkAuth() error {
	return m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
		path, cleanup, err := writeLinuxLinkSecret(linuxLinkRuntimeDir(), secret)
		if err != nil {
			return nil, err
		}
		return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET_FILE=" + path}, cleanup: cleanup}, nil
	})
}

// Called only after trusted session ownership and observed bridge teardown.
func (m *Manager) removeRecoveredLinkAuth() error {
	if m.paths.Mode != ModeInteractive {
		return nil
	}
	linuxLinkSecretMu.Lock()
	defer linuxLinkSecretMu.Unlock()
	path := filepath.Join(linuxLinkRuntimeDir(), "solarbeam-link-auth")
	// No provision was created on this installation; do not create one at stop.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	fd, err := openLinkDirectory(path, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := validLinkFile(fd, "secret")
	if err != nil || current == nil {
		return err
	}
	if unix.Unlinkat(fd, "secret", 0) != nil {
		return errLinkCredential
	}
	return nil
}
