//go:build linux

package trayicon

import (
	"errors"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

type notifierItem struct{ state *linuxTrayState }

func (n *notifierItem) Activate(x, y int32) *dbus.Error {
	return n.state.enqueue(trayEvent{activate: true})
}
func (n *notifierItem) SecondaryActivate(x, y int32) *dbus.Error {
	return n.state.enqueue(trayEvent{activate: true})
}

// The host renders our Menu property using DBusMenu. A host that instead asks
// us to render a native context menu can still reach the companion via activate.
func (n *notifierItem) ContextMenu(x, y int32) *dbus.Error {
	return n.state.enqueue(trayEvent{activate: true})
}
func (n *notifierItem) Scroll(delta int32, orientation string) *dbus.Error { return nil }

type toolTip struct {
	IconName    string
	IconPixmap  []iconPixmap
	Title       string
	Description string
}
type trayProperties struct {
	state *linuxTrayState
	menu  bool
}

func (p *trayProperties) values() map[string]interface{} {
	if p.menu {
		return map[string]interface{}{"Version": uint32(3), "TextDirection": "ltr", "Status": "normal", "IconThemePath": []string{}}
	}
	p.state.mu.RLock()
	defer p.state.mu.RUnlock()
	icon := p.state.icon
	if p.state.badge {
		icon = withBadge(icon)
	}
	return map[string]interface{}{
		"Category": "ApplicationStatus", "Id": "readyfleet", "Title": "ReadyFleet", "Status": "Active",
		"WindowId": uint32(0), "IconName": "", "IconPixmap": icon, "OverlayIconName": "", "OverlayIconPixmap": []iconPixmap{},
		"AttentionIconName": "", "AttentionIconPixmap": []iconPixmap{}, "AttentionMovieName": "",
		"ToolTip": toolTip{"", icon, "ReadyFleet", p.state.tooltip}, "ItemIsMenu": p.state.activate == nil, "Menu": menuPath,
	}
}
func (p *trayProperties) validInterface(name string) bool {
	if p.menu {
		return name == menuInterface
	}
	return name == itemInterface
}
func (p *trayProperties) GetAll(name string) (map[string]dbus.Variant, *dbus.Error) {
	if !p.validInterface(name) {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", []interface{}{name})
	}
	out := map[string]dbus.Variant{}
	for key, value := range p.values() {
		out[key] = dbus.MakeVariant(value)
	}
	return out, nil
}
func (p *trayProperties) Get(name, key string) (dbus.Variant, *dbus.Error) {
	values, err := p.GetAll(name)
	if err != nil {
		return dbus.Variant{}, err
	}
	value, ok := values[key]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []interface{}{key})
	}
	return value, nil
}
func (p *trayProperties) Set(name, key string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []interface{}{key})
}
func itemIntrospection() []introspect.Property {
	out := []introspect.Property{}
	state := &linuxTrayState{icon: []iconPixmap{}}
	for name, value := range (&trayProperties{state, false}).values() {
		out = append(out, introspect.Property{Name: name, Type: dbus.SignatureOf(value).String(), Access: "read"})
	}
	return out
}

type menuLayout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}
type menuProperties struct {
	ID         int32
	Properties map[string]dbus.Variant
}
type menuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}
type dbusMenu struct{ state *linuxTrayState }

func filterProperties(values map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return values
	}
	out := map[string]dbus.Variant{}
	for _, name := range names {
		if value, ok := values[name]; ok {
			out[name] = value
		}
	}
	return out
}
func propertiesFor(item MenuItem) map[string]dbus.Variant {
	values := map[string]dbus.Variant{"visible": dbus.MakeVariant(true)}
	if item.Separator {
		values["type"] = dbus.MakeVariant("separator")
		return values
	}
	values["label"] = dbus.MakeVariant(item.Label)
	values["enabled"] = dbus.MakeVariant(!item.Disabled)
	// The shared API has Checked but no checkable field. Export an unchecked
	// checkbox too, matching the Windows tray's available checkmark column.
	values["toggle-type"] = dbus.MakeVariant("checkmark")
	checked := int32(0)
	if item.Checked {
		checked = 1
	}
	values["toggle-state"] = dbus.MakeVariant(checked)
	return values
}
func (m *dbusMenu) GetLayout(parent, depth int32, names []string) (uint32, menuLayout, *dbus.Error) {
	m.state.mu.RLock()
	defer m.state.mu.RUnlock()
	root := menuLayout{ID: parent, Properties: map[string]dbus.Variant{}, Children: []dbus.Variant{}}
	if parent == 0 {
		root.Properties = filterProperties(map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, names)
		if depth != 0 {
			for _, item := range m.state.items {
				root.Children = append(root.Children, dbus.MakeVariant(menuLayout{item.ID, filterProperties(propertiesFor(item), names), []dbus.Variant{}}))
			}
		}
		return m.state.revision, root, nil
	}
	for _, item := range m.state.items {
		if item.ID == parent {
			root.Properties = filterProperties(propertiesFor(item), names)
			return m.state.revision, root, nil
		}
	}
	return m.state.revision, root, dbus.MakeFailedError(errors.New("unknown menu item"))
}
func (m *dbusMenu) GetGroupProperties(ids []int32, names []string) ([]menuProperties, *dbus.Error) {
	m.state.mu.RLock()
	defer m.state.mu.RUnlock()
	out := []menuProperties{}
	for _, item := range m.state.items {
		include := len(ids) == 0
		for _, id := range ids {
			if item.ID == id {
				include = true
				break
			}
		}
		if include {
			out = append(out, menuProperties{item.ID, filterProperties(propertiesFor(item), names)})
		}
	}
	for _, id := range ids {
		if id == 0 {
			out = append(out, menuProperties{0, filterProperties(map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, names)})
			break
		}
	}
	return out, nil
}
func (m *dbusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	values, _ := m.GetGroupProperties([]int32{id}, []string{name})
	for _, value := range values {
		if property, ok := value.Properties[name]; ok {
			return property, nil
		}
	}
	return dbus.Variant{}, dbus.MakeFailedError(errors.New("unknown menu property"))
}
func (m *dbusMenu) Event(id int32, event string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if event != "clicked" {
		return nil
	}
	if m.state.callback(trayEvent{id: id}) == nil {
		return dbus.MakeFailedError(errors.New("menu item is unavailable"))
	}
	return m.state.enqueue(trayEvent{id: id})
}
func (m *dbusMenu) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	failed := []int32{}
	for _, event := range events {
		if err := m.Event(event.ID, event.EventID, event.Data, event.Timestamp); err != nil {
			failed = append(failed, event.ID)
		}
	}
	return failed, nil
}
func (m *dbusMenu) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }
func (m *dbusMenu) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}
