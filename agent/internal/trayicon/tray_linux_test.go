//go:build linux

package trayicon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/desktopnotify"
)

func testTrayState(t *testing.T, items []MenuItem) *linuxTrayState {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &linuxTrayState{ctx: ctx, cancel: cancel, items: copyMenu(items), revision: 1, icon: loadTrayIcon(""), events: make(chan trayEvent, 32), changed: make(chan struct{}, 1)}
}
func TestLinuxMenuWireShapes(t *testing.T) {
	s := testTrayState(t, []MenuItem{{ID: 1, Label: "Connected", Disabled: true}, {Separator: true}, {ID: 2, Label: "Stop sharing", Checked: true, OnClick: func() {}}})
	m := dbusMenu{s}
	revision, layout, err := m.GetLayout(0, -1, nil)
	if err != nil || revision != 1 || len(layout.Children) != 3 {
		t.Fatalf("layout: %d %#v %v", revision, layout, err)
	}
	for _, test := range []struct {
		value     interface{}
		signature string
	}{
		{layout, "(ia{sv}av)"}, {[]menuProperties{}, "a(ia{sv})"}, {[]menuEvent{}, "a(isvu)"},
		{toolTip{"", s.icon, "ReadyFleet", "Connected"}, "(sa(iiay)ss)"},
	} {
		if got := dbus.SignatureOf(test.value).String(); got != test.signature {
			t.Errorf("%T signature=%s want=%s", test.value, got, test.signature)
		}
	}
	first := layout.Children[0].Value().(menuLayout)
	if first.Properties["enabled"].Value() != false {
		t.Error("disabled status rendered enabled")
	}
	separator := layout.Children[1].Value().(menuLayout)
	if separator.ID >= 0 || separator.Properties["type"].Value() != "separator" {
		t.Error("separator not mapped to private ID")
	}
	checked := layout.Children[2].Value().(menuLayout)
	if checked.Properties["toggle-state"].Value() != int32(1) {
		t.Error("check state missing")
	}
	_, shallow, _ := m.GetLayout(0, 0, nil)
	if len(shallow.Children) != 0 {
		t.Error("depth zero included children")
	}
	_, filtered, _ := m.GetLayout(2, -1, []string{"label"})
	if len(filtered.Properties) != 1 {
		t.Errorf("filter ignored: %#v", filtered.Properties)
	}
	if _, _, err := m.GetLayout(99, -1, nil); err == nil {
		t.Error("unknown ID accepted")
	}
}
func TestLinuxQueuedClickRechecksCurrentMenu(t *testing.T) {
	var calls atomic.Int32
	s := testTrayState(t, []MenuItem{{ID: 7, Label: "Disconnect", OnClick: func() { calls.Add(1) }}})
	menu := dbusMenu{s}
	if err := menu.Event(7, "clicked", dbus.MakeVariant(int32(0)), 0); err != nil {
		t.Fatal(err)
	}
	// A fresh snapshot revoked this action while the UI click waited in queue.
	s.mu.Lock()
	s.items[0].Disabled = true
	s.mu.Unlock()
	event := <-s.events
	if callback := s.callback(event); callback != nil {
		callback()
	}
	if calls.Load() != 0 {
		t.Fatal("stale queued action executed after revocation")
	}
	if err := menu.Event(7, "clicked", dbus.MakeVariant(0), 0); err == nil {
		t.Fatal("disabled action accepted")
	}
	if err := menu.Event(999, "clicked", dbus.MakeVariant(0), 0); err == nil {
		t.Fatal("unknown action accepted")
	}
}
func TestLinuxMenuCopiesAndRejectsAmbiguousIDs(t *testing.T) {
	input := []MenuItem{{ID: 0, Label: "root collision"}, {ID: 5, Label: "first"}, {ID: 5, Label: "duplicate"}, {Separator: true}, {Separator: true}}
	output := copyMenu(input)
	input[1].Label = "mutated"
	if len(output) != 3 || output[0].Label != "first" || output[1].ID == output[2].ID {
		t.Fatalf("unsafe menu %#v", output)
	}
}
func TestLinuxQueueBoundedAndCancelled(t *testing.T) {
	s := testTrayState(t, nil)
	for n := 0; n < cap(s.events); n++ {
		if err := s.enqueue(trayEvent{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.enqueue(trayEvent{}); err == nil {
		t.Fatal("overflow did not refuse")
	}
	s.cancel()
	if err := s.enqueue(trayEvent{}); err == nil {
		t.Fatal("closed tray accepted action")
	}
}
func TestLinuxTrayActivationAndQuit(t *testing.T) {
	s := testTrayState(t, nil)
	called := make(chan struct{})
	s.activate = func() { close(called) }
	go s.dispatch()
	if err := (&notifierItem{s}).Activate(100, 200); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("activate callback not dispatched")
	}
	tray := &Tray{state: s}
	tray.Quit()
	if s.ctx.Err() == nil {
		t.Fatal("Quit did not cancel lifecycle")
	}
}
func TestLinuxRunDialFailureCleansState(t *testing.T) {
	wanted := errors.New("no login bus")
	tray := &Tray{dial: func() (*dbus.Conn, error) { return nil, wanted }}
	if err := tray.Run(context.Background(), nil); !errors.Is(err, wanted) {
		t.Fatalf("error=%v", err)
	}
	if tray.state != nil {
		t.Fatal("failed start retained live state")
	}
	tray.Quit()
	tray.dial = func() (*dbus.Conn, error) { t.Fatal("Quit-before-Run attempted login bus"); return nil, nil }
	if err := tray.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
func TestLinuxWatcherRestartRecognition(t *testing.T) {
	for _, test := range []struct {
		signal            *dbus.Signal
		changed, appeared bool
	}{
		{nil, false, false},
		{&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []interface{}{watcherName, "", ":1.55"}}, true, true},
		{&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []interface{}{watcherName, ":1.55", ""}}, true, false},
		{&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []interface{}{"unrelated", "", ":1.55"}}, false, false},
	} {
		if watcherChanged(test.signal) != test.changed || watcherAppeared(test.signal) != test.appeared {
			t.Errorf("bad signal result %#v", test)
		}
	}
}
func TestLinuxTrayPropertiesReadOnlyAndBadge(t *testing.T) {
	s := testTrayState(t, nil)
	s.tooltip = "Connected"
	p := trayProperties{s, false}
	all, err := p.GetAll(itemInterface)
	if err != nil {
		t.Fatal(err)
	}
	if all["Menu"].Value() != menuPath || all["Status"].Value() != "Active" {
		t.Fatal("item properties missing")
	}
	if _, err := p.Get("bad", "Menu"); err == nil {
		t.Fatal("wrong interface accepted")
	}
	if _, err := p.Get(itemInterface, "not a property"); err == nil {
		t.Fatal("unknown property accepted")
	}
	if err := p.Set(itemInterface, "Status", dbus.MakeVariant("Passive")); err == nil {
		t.Fatal("property write accepted")
	}
	original := append([]byte(nil), s.icon[0].Data...)
	tray := Tray{state: s}
	_ = tray.SetBadge(true)
	icon, err := p.Get(itemInterface, "IconPixmap")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(icon.Value().([]iconPixmap)[0].Data, original) {
		t.Fatal("badge had no visible effect")
	}
	if !bytes.Equal(s.icon[0].Data, original) {
		t.Fatal("badge mutated base icon")
	}
	_ = tray.SetBadge(false)
	icon, _ = p.Get(itemInterface, "IconPixmap")
	if !bytes.Equal(icon.Value().([]iconPixmap)[0].Data, original) {
		t.Fatal("badge did not clear")
	}
}
func TestLinuxInstalledPNGICOAndARGB(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 20, G: 40, B: 60, A: 255})
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	ico := make([]byte, 22)
	binary.LittleEndian.PutUint16(ico[2:], 1)
	binary.LittleEndian.PutUint16(ico[4:], 1)
	binary.LittleEndian.PutUint32(ico[14:], uint32(pngData.Len()))
	binary.LittleEndian.PutUint32(ico[18:], 22)
	ico = append(ico, pngData.Bytes()...)
	decoded := decodeTrayImage(ico)
	if decoded == nil {
		t.Fatal("PNG-bearing ICO not decoded")
	}
	pixmap := imagePixmap(decoded)
	if !bytes.Equal(pixmap.Data[:4], []byte{255, 20, 40, 60}) {
		t.Fatalf("not network ARGB: %v", pixmap.Data[:4])
	}
	binary.LittleEndian.PutUint32(ico[18:], 0xffffffff)
	if decodeTrayImage(ico) != nil {
		t.Fatal("out-of-bounds ICO frame accepted")
	}
}

// Notification tests never connect to a desktop, including the callback lane.
type fakeTrayNotifications struct {
	events       chan desktopnotify.Event
	closed       chan struct{}
	notification desktopnotify.Notification
}

func (f *fakeTrayNotifications) Notify(ctx context.Context, n desktopnotify.Notification) (uint32, error) {
	f.notification = n
	return 42, nil
}
func (f *fakeTrayNotifications) Close(context.Context, uint32) error { return nil }
func (f *fakeTrayNotifications) Events() <-chan desktopnotify.Event  { return f.events }
func (f *fakeTrayNotifications) Shutdown() error                     { close(f.closed); return nil }
func TestLinuxNotificationClickMatchesIDAndAction(t *testing.T) {
	s := testTrayState(t, nil)
	fake := &fakeTrayNotifications{events: make(chan desktopnotify.Event, 3), closed: make(chan struct{})}
	tray := Tray{state: s, notifyDial: func() (desktopnotify.Session, error) { return fake, nil }}
	clicked := make(chan struct{})
	if err := tray.Notify("ReadyFleet", "A colleague connected", NotifyInfo, func() { close(clicked) }); err != nil {
		t.Fatal(err)
	}
	fake.events <- desktopnotify.Event{Kind: desktopnotify.EventAction, ID: 99, ActionKey: "default"}
	fake.events <- desktopnotify.Event{Kind: desktopnotify.EventAction, ID: 42, ActionKey: "not-default"}
	fake.events <- desktopnotify.Event{Kind: desktopnotify.EventAction, ID: 42, ActionKey: "default"}
	select {
	case <-clicked:
	case <-time.After(time.Second):
		t.Fatal("notice click missing")
	}
	select {
	case <-fake.closed:
	case <-time.After(time.Second):
		t.Fatal("notification session leaked")
	}
}
func TestLinuxNotificationCancelledWithoutAction(t *testing.T) {
	s := testTrayState(t, nil)
	fake := &fakeTrayNotifications{events: make(chan desktopnotify.Event), closed: make(chan struct{})}
	tray := Tray{state: s, notifyDial: func() (desktopnotify.Session, error) { return fake, nil }}
	if err := tray.Notify("ReadyFleet", "Notice", NotifyInfo, func() { t.Error("cancelled notification invoked callback") }); err != nil {
		t.Fatal(err)
	}
	s.cancel()
	select {
	case <-fake.closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled notification session leaked")
	}
}

func TestLinuxMenuAndTooltipUpdatesInvalidateHostCache(t *testing.T) {
	s := testTrayState(t, []MenuItem{{ID: 1, Label: "Old"}})
	tray := Tray{state: s}
	tray.SetMenuItems([]MenuItem{{ID: 2, Label: "New", Disabled: true}})
	tray.SetTooltip("Reconnecting")
	revision, layout, err := (&dbusMenu{s}).GetLayout(0, -1, nil)
	if err != nil || revision != 2 || len(layout.Children) != 1 || layout.Children[0].Value().(menuLayout).ID != 2 {
		t.Fatalf("host sees stale menu: %d %#v %v", revision, layout, err)
	}
	props := trayProperties{s, false}
	value, _ := props.Get(itemInterface, "ToolTip")
	if value.Value().(toolTip).Description != "Reconnecting" {
		t.Fatal("host sees stale tooltip")
	}
	select {
	case <-s.changed:
	default:
		t.Fatal("host not notified of change")
	}
}
