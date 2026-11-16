//go:build linux

package solarbeam

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveDisplayEnv(t *testing.T) {
	cases := []struct {
		name       string
		process    map[string]string
		manager    map[string]string
		sockets    []string
		want       []string
		wantNoVars []string
	}{
		{
			// The failure this whole change exists for: the agent started
			// before the compositor, so its own environ has nothing, and the
			// systemd user manager is the only source that knows.
			name:    "agent started before the session, manager knows",
			process: map[string]string{},
			manager: map[string]string{
				"WAYLAND_DISPLAY":  "wayland-0",
				"DISPLAY":          ":0",
				"XAUTHORITY":       "/run/user/1000/xauth_dlINlR",
				"XDG_SESSION_TYPE": "wayland",
			},
			want: []string{
				"WAYLAND_DISPLAY=wayland-0",
				"DISPLAY=:0",
				"XAUTHORITY=/run/user/1000/xauth_dlINlR",
				"XDG_SESSION_TYPE=wayland",
			},
		},
		{
			name:    "the manager wins over a stale inherited value",
			process: map[string]string{"WAYLAND_DISPLAY": "wayland-9", "DISPLAY": ":0"},
			manager: map[string]string{"WAYLAND_DISPLAY": "wayland-1"},
			want:    []string{"WAYLAND_DISPLAY=wayland-1", "DISPLAY=:0"},
		},
		{
			// A source with nothing to say must not erase a good value.
			name:    "an empty manager value does not clobber inheritance",
			process: map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			manager: map[string]string{"WAYLAND_DISPLAY": "", "DISPLAY": "   "},
			want:    []string{"WAYLAND_DISPLAY=wayland-0"},
		},
		{
			name:    "socket probe is the last resort",
			process: map[string]string{},
			manager: map[string]string{},
			sockets: []string{"wayland-0", "wayland-1"},
			want:    []string{"WAYLAND_DISPLAY=wayland-0", "XDG_SESSION_TYPE=wayland"},
		},
		{
			name:    "the socket probe never overrides a known display",
			process: map[string]string{"WAYLAND_DISPLAY": "wayland-2", "XDG_SESSION_TYPE": "wayland"},
			manager: map[string]string{},
			sockets: []string{"wayland-0"},
			want:    []string{"WAYLAND_DISPLAY=wayland-2", "XDG_SESSION_TYPE=wayland"},
		},
		{
			// An X11 session has no wayland socket and must not be told it is
			// a wayland session.
			name:    "pure X11 session",
			process: map[string]string{"DISPLAY": ":0", "XAUTHORITY": "/home/m/.Xauthority", "XDG_SESSION_TYPE": "x11"},
			manager: map[string]string{},
			want:    []string{"DISPLAY=:0", "XAUTHORITY=/home/m/.Xauthority", "XDG_SESSION_TYPE=x11"},
		},
		{
			name:    "no session anywhere yields nothing rather than a wrong guess",
			process: map[string]string{},
			manager: map[string]string{},
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDisplayEnv(tc.process, tc.manager, tc.sockets)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveDisplayEnv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnquoteSystemdValue(t *testing.T) {
	cases := map[string]string{
		`wayland-0`:                    "wayland-0",
		`"/run/user/1000/xauth_dlIN"`:  "/run/user/1000/xauth_dlIN",
		`"a \"quoted\" path"`:          `a "quoted" path`,
		`'single'`:                     "single",
		`/usr/share/wayland-sessions/`: "/usr/share/wayland-sessions/",
		``:                             "",
	}
	for in, want := range cases {
		if got := unquoteSystemdValue(in); got != want {
			t.Errorf("unquoteSystemdValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWaylandSocketsInSkipsLockFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"wayland-1", "wayland-1.lock", "wayland-0", "wayland-0.lock", "bus", "kwin-xwayland-eis-socket.171222.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := waylandSocketsIn(dir)
	want := []string{"wayland-0", "wayland-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waylandSocketsIn = %q, want %q", got, want)
	}
}

func TestWaylandSocketsInHandlesAMissingRuntimeDir(t *testing.T) {
	if got := waylandSocketsIn(""); got != nil {
		t.Fatalf("waylandSocketsIn(\"\") = %q, want nil", got)
	}
	if got := waylandSocketsIn(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Fatalf("waylandSocketsIn(missing) = %q, want nil", got)
	}
}
