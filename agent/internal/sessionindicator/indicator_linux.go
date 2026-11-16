//go:build linux

package sessionindicator

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/rajveermalviya/go-wayland/wayland/client"
	"golang.org/x/sys/unix"
)

// setupTimeout bounds the whole Wayland handshake — connect, bind, one
// configure round-trip per output. A compositor that never answers must
// surface as "the indicator could not be shown", not as a Show() that
// blocks the session-start path forever.
const setupTimeout = 5 * time.Second

// namespace is the layer-shell role string KWin sees. Distinctive on
// purpose: it is what an operator greps for in a compositor log, and what
// any future KWin window rule would match on.
const namespace = "readyfleet-session-indicator"

// New returns the Linux indicator. It does not touch the compositor —
// nothing is connected to until the first Show, so an agent that never
// hosts a session never opens a Wayland connection.
func New() (Indicator, error) {
	return newSupervisor(startLinuxBar, platformScreenState, slog.Default()), nil
}

// startLinuxBar brings up one bar per output over a dedicated Wayland
// connection, and returns once every bar is actually mapped.
//
// A whole connection per Show is deliberate. go-wayland's client.Context
// is not safe for concurrent use — Dispatch reads the object map that
// Register writes — and Dispatch blocks on the socket with no way to
// interrupt it, since the library exposes neither the fd nor a deadline.
// Owning the connection means every Wayland call happens on this bar's
// own goroutine, and the one cross-goroutine operation is closing the
// net.UnixConn, which unblocks the reader and is safe by construction.
// Shows are seconds apart at worst, so the cost is irrelevant.
func startLinuxBar(n Notice) (bar, error) {
	b := &linuxBar{done: make(chan struct{})}
	if err := b.setup(n); err != nil {
		b.Close()  // drop the connection; the compositor cleans up whatever was made
		b.finish() // nobody will run the dispatch loop, so release here
		return nil, err
	}
	go b.run()
	return b, nil
}

type linuxBar struct {
	display *client.Display
	shell   *LayerShell
	bars    []*linuxSurface

	done      chan struct{}
	closeOnce sync.Once
	runOnce   sync.Once

	// protoErr records a wl_display.error, which is far more useful than
	// the "connection reset" the socket reports a moment later.
	protoErr error
}

type linuxSurface struct {
	surface *client.Surface
	layer   *LayerSurface
	output  *client.Output
	scale   int32

	configured bool
	width      int32
	height     int32

	shmFile *os.File
	mapped  []byte
}

func (b *linuxBar) Done() <-chan struct{} { return b.done }

// Close drops the Wayland connection. The compositor destroys every
// surface this client made, so there is no ordered teardown to get wrong
// and nothing left on screen. Closing the net.UnixConn is also the only
// way to unblock a Dispatch already waiting on the socket, which is why
// it is the single cross-goroutine operation this backend performs. Safe
// from any goroutine and idempotent.
func (b *linuxBar) Close() {
	b.closeOnce.Do(func() {
		if b.display != nil {
			_ = b.display.Context().Close()
		}
	})
}

// run pumps events until the connection dies, then releases the shm
// mappings and signals Done. The supervisor turns that into a re-assert.
func (b *linuxBar) run() {
	defer b.finish()
	for {
		if err := b.display.Context().Dispatch(); err != nil {
			return
		}
	}
}

func (b *linuxBar) finish() {
	b.runOnce.Do(func() {
		for _, s := range b.bars {
			if s.mapped != nil {
				_ = unix.Munmap(s.mapped)
				s.mapped = nil
			}
			if s.shmFile != nil {
				_ = s.shmFile.Close()
				s.shmFile = nil
			}
		}
		close(b.done)
	})
}

func (b *linuxBar) setup(n Notice) (err error) {
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("WAYLAND_SOCKET") == "" {
		// An X11 session, a headless service account, or a console login.
		// Named explicitly rather than left to a socket-dial error,
		// because "there is no Wayland session here" is a policy-relevant
		// answer and "no such file or directory" is not.
		return errors.New("no Wayland session (WAYLAND_DISPLAY unset); the indicator needs Wayland + zwlr_layer_shell_v1")
	}

	display, connErr := client.Connect("")
	if connErr != nil {
		return fmt.Errorf("connect to Wayland: %w", connErr)
	}
	b.display = display
	display.SetErrorHandler(func(e client.DisplayErrorEvent) {
		b.protoErr = fmt.Errorf("wayland protocol error: object %d code %d: %s", e.ObjectId, e.Code, e.Message)
	})

	// Watchdog: closing the connection is the only way to interrupt a
	// blocked Dispatch, so a compositor that stops answering becomes an
	// error return rather than a hang. Stopped the moment setup is done.
	watchdog := time.AfterFunc(setupTimeout, b.Close)
	defer func() {
		if !watchdog.Stop() {
			err = errors.Join(err, fmt.Errorf("timed out after %s waiting for the compositor", setupTimeout))
		}
		if err != nil && b.protoErr != nil {
			err = errors.Join(err, b.protoErr)
		}
	}()

	registry, err := display.GetRegistry()
	if err != nil {
		return fmt.Errorf("get wl_registry: %w", err)
	}

	var (
		compositor *client.Compositor
		shm        *client.Shm
		outputs    []*linuxSurface
		bindErr    error
	)
	registry.SetGlobalHandler(func(e client.RegistryGlobalEvent) {
		if bindErr != nil {
			return
		}
		switch e.Interface {
		case "wl_compositor":
			compositor = client.NewCompositor(display.Context())
			bindErr = bindGlobal(registry, e.Name, e.Interface, min(e.Version, 4), compositor)
		case "wl_shm":
			shm = client.NewShm(display.Context())
			bindErr = bindGlobal(registry, e.Name, e.Interface, min(e.Version, 1), shm)
		case layerShellInterface:
			b.shell = NewLayerShell(display.Context())
			bindErr = bindGlobal(registry, e.Name, e.Interface, min(e.Version, layerShellVersion), b.shell)
		case "wl_output":
			out := client.NewOutput(display.Context())
			if err := bindGlobal(registry, e.Name, e.Interface, min(e.Version, 4), out); err != nil {
				bindErr = err
				return
			}
			s := &linuxSurface{output: out, scale: 1}
			// wl_output.scale (since v2) is the integer buffer scale for
			// this screen. Without honouring it the bar renders at half
			// size on a HiDPI monitor while the engine still reserves
			// ReserveHeight logical pixels — a visible, wrong-looking gap
			// above the desktop.
			out.SetScaleHandler(func(e client.OutputScaleEvent) {
				if e.Factor >= 1 {
					s.scale = e.Factor
				}
			})
			outputs = append(outputs, s)
		}
	})

	if err := b.roundtrip(); err != nil {
		return err
	}
	if bindErr != nil {
		return fmt.Errorf("bind Wayland global: %w", bindErr)
	}
	// A second round-trip: wl_output's geometry/scale/done burst arrives
	// after the bind, not with the global announcement.
	if err := b.roundtrip(); err != nil {
		return err
	}

	switch {
	case compositor == nil:
		return errors.New("compositor advertises no wl_compositor")
	case shm == nil:
		return errors.New("compositor advertises no wl_shm")
	case b.shell == nil:
		return fmt.Errorf("compositor advertises no %s; the indicator cannot be placed where the capture excludes it", layerShellInterface)
	case len(outputs) == 0:
		return errors.New("compositor advertises no wl_output")
	}

	for _, s := range outputs {
		if err := b.createSurface(compositor, s); err != nil {
			return err
		}
	}
	b.bars = outputs

	// A layer surface is not mapped until it has acked a configure AND
	// committed a buffer, and the configure arrives on the compositor's
	// own schedule — not necessarily inside the round-trip that follows
	// the commit. So block on the event itself rather than on a sync, and
	// let the watchdog turn "never configured" into an error.
	if err := b.waitUntil("zwlr_layer_surface_v1.configure", func() bool {
		for _, s := range b.bars {
			if !s.configured {
				return false
			}
		}
		return true
	}); err != nil {
		return err
	}
	for _, s := range b.bars {
		if err := b.paint(shm, s, n); err != nil {
			return err
		}
	}
	return b.roundtrip()
}

// waitUntil pumps events until pred holds. Unbounded on purpose: the
// setup watchdog closes the connection, which makes Dispatch return an
// error, which is how this stops if the compositor goes quiet.
func (b *linuxBar) waitUntil(what string, pred func() bool) error {
	for !pred() {
		if err := b.display.Context().Dispatch(); err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
	}
	return nil
}

// createSurface builds one anchored, click-through, exclusive-zone bar on
// one output and commits it empty so the compositor answers with a
// configure carrying the real width.
func (b *linuxBar) createSurface(compositor *client.Compositor, s *linuxSurface) error {
	surface, err := compositor.CreateSurface()
	if err != nil {
		return fmt.Errorf("create wl_surface: %w", err)
	}
	s.surface = surface

	// An EMPTY input region, not a missing one: by default a surface's
	// input region is infinite, so without this the bar would swallow
	// every click in the top 40 pixels of the screen. Empty means every
	// pointer and touch event falls through to whatever is underneath,
	// which is also why the bar cannot be dragged, focused or dismissed.
	region, err := compositor.CreateRegion()
	if err != nil {
		return fmt.Errorf("create wl_region: %w", err)
	}
	if err := surface.SetInputRegion(region); err != nil {
		return fmt.Errorf("set empty input region: %w", err)
	}
	if err := region.Destroy(); err != nil {
		return fmt.Errorf("destroy wl_region: %w", err)
	}

	layer, err := b.shell.GetLayerSurface(surface, s.output, layerOverlay, namespace)
	if err != nil {
		return fmt.Errorf("get zwlr_layer_surface_v1: %w", err)
	}
	s.layer = layer
	layer.SetConfigureHandler(func(e LayerSurfaceConfigureEvent) {
		_ = layer.AckConfigure(e.Serial)
		s.width, s.height = int32(e.Width), int32(e.Height)
		if s.height <= 0 {
			s.height = ReserveHeight
		}
		s.configured = true
	})
	layer.SetClosedHandler(func(LayerSurfaceClosedEvent) {
		// The compositor took the bar away while a session is live. Drop
		// the connection so the supervisor sees Done and puts it back —
		// this is the "cannot be dismissed" guarantee, not a shutdown.
		b.Close()
	})

	// Width 0 with LEFT|RIGHT anchoring is "as wide as the output".
	if err := layer.SetSize(0, ReserveHeight); err != nil {
		return fmt.Errorf("set_size: %w", err)
	}
	if err := layer.SetAnchor(anchorTop | anchorLeft | anchorRight); err != nil {
		return fmt.Errorf("set_anchor: %w", err)
	}
	if err := layer.SetExclusiveZone(ReserveHeight); err != nil {
		return fmt.Errorf("set_exclusive_zone: %w", err)
	}
	if err := layer.SetKeyboardInteractivity(keyboardInteractivityNone); err != nil {
		return fmt.Errorf("set_keyboard_interactivity: %w", err)
	}
	if err := surface.SetBufferScale(s.scale); err != nil {
		return fmt.Errorf("set_buffer_scale: %w", err)
	}
	if err := surface.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// paint renders the bar at this output's scale and attaches it.
func (b *linuxBar) paint(shm *client.Shm, s *linuxSurface, n Notice) error {
	if s.scale < 1 {
		s.scale = 1
	}
	pw := int(s.width * s.scale)
	ph := int(s.height * s.scale)
	if pw < 1 || ph < 1 {
		return fmt.Errorf("compositor configured a %dx%d layer surface", s.width, s.height)
	}
	pix := toBGRA(renderBar(pw, ph, n))

	size := len(pix)
	f, err := shmFile(int64(size))
	if err != nil {
		return err
	}
	s.shmFile = f
	mem, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap shm buffer: %w", err)
	}
	s.mapped = mem
	copy(mem, pix)

	pool, err := shm.CreatePool(int(f.Fd()), int32(size))
	if err != nil {
		return fmt.Errorf("wl_shm.create_pool: %w", err)
	}
	// ShmFormatArgb8888 is little-endian 0xAARRGGBB, i.e. B,G,R,A in
	// memory — exactly what toBGRA produces.
	buf, err := pool.CreateBuffer(0, int32(pw), int32(ph), int32(pw*4), uint32(client.ShmFormatArgb8888))
	if err != nil {
		return fmt.Errorf("wl_shm_pool.create_buffer: %w", err)
	}
	if err := pool.Destroy(); err != nil {
		return fmt.Errorf("wl_shm_pool.destroy: %w", err)
	}
	if err := s.surface.Attach(buf, 0, 0); err != nil {
		return fmt.Errorf("wl_surface.attach: %w", err)
	}
	if err := s.surface.Damage(0, 0, s.width, s.height); err != nil {
		return fmt.Errorf("wl_surface.damage: %w", err)
	}
	return s.surface.Commit()
}

// roundtrip drives the event loop until the compositor has processed
// everything sent so far. Errors here mean the connection is gone —
// including the watchdog having closed it.
func (b *linuxBar) roundtrip() error {
	cb, err := b.display.Sync()
	if err != nil {
		return fmt.Errorf("wl_display.sync: %w", err)
	}
	done := false
	cb.SetDoneHandler(func(client.CallbackDoneEvent) { done = true })
	for !done {
		if err := b.display.Context().Dispatch(); err != nil {
			return fmt.Errorf("wayland dispatch: %w", err)
		}
	}
	return nil
}

// shmFile returns an anonymous, sized file suitable for wl_shm. memfd is
// preferred (never touches a filesystem, cannot be found or opened by
// another process by name); the XDG_RUNTIME_DIR fallback covers a kernel
// or sandbox without memfd_create, and unlinks immediately so it is just
// as anonymous once open.
func shmFile(size int64) (*os.File, error) {
	fd, err := unix.MemfdCreate("readyfleet-session-indicator", unix.MFD_CLOEXEC)
	if err == nil {
		f := os.NewFile(uintptr(fd), "readyfleet-session-indicator")
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("size memfd shm buffer: %w", err)
		}
		return f, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return nil, fmt.Errorf("memfd_create: %w (and XDG_RUNTIME_DIR is unset for the fallback)", err)
	}
	f, ferr := os.CreateTemp(dir, "readyfleet-indicator-*")
	if ferr != nil {
		return nil, fmt.Errorf("create shm buffer: %w", ferr)
	}
	_ = os.Remove(f.Name())
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("size shm buffer: %w", err)
	}
	return f, nil
}
