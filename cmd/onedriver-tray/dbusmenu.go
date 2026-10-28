package main

import (
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// Minimal implementation of the com.canonical.dbusmenu server side, which is
// what StatusNotifierItem hosts (KDE Plasma, etc.) use to render the menu.

const (
	menuPath  = dbus.ObjectPath("/MenuBar")
	menuIface = "com.canonical.dbusmenu"
)

type menuLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

type menuProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

type menuEvent struct {
	ID      int32
	EventID string
	Data    dbus.Variant
	Time    uint32
}

type dbusMenu struct {
	conn     *dbus.Conn
	onAction func(Action)
	onShow   func() bool

	mu    sync.Mutex
	rev   uint32
	items []*MenuItem
	idx   map[int32]*MenuItem
}

func newDBusMenu(conn *dbus.Conn, onAction func(Action), onShow func() bool) (*dbusMenu, error) {
	m := &dbusMenu{conn: conn, onAction: onAction, onShow: onShow, idx: map[int32]*MenuItem{}}
	if err := conn.Export(m, menuPath, menuIface); err != nil {
		return nil, err
	}
	props, err := prop.Export(conn, menuPath, map[string]map[string]*prop.Prop{menuIface: {
		"Version":        {Value: uint32(3)},
		"TextDirection":  {Value: "ltr"},
		"Status":         {Value: "normal"},
		"IconThemePaths": {Value: []string{}},
	}})
	if err != nil {
		return nil, err
	}
	node := &introspect.Node{Name: string(menuPath), Interfaces: []introspect.Interface{
		introspect.IntrospectData, prop.IntrospectData,
		{Name: menuIface, Methods: introspect.Methods(m), Properties: props.Introspection(menuIface)},
	}}
	return m, conn.Export(introspect.NewIntrospectable(node), menuPath, "org.freedesktop.DBus.Introspectable")
}

// SetItems replaces the menu and announces the new layout.
func (m *dbusMenu) SetItems(items []*MenuItem) {
	m.mu.Lock()
	m.idx = AssignIDs(items)
	m.items = items
	m.rev++
	rev := m.rev
	m.mu.Unlock()
	m.conn.Emit(menuPath, menuIface+".LayoutUpdated", rev, int32(0))
}

func itemProps(it *MenuItem, names []string) map[string]dbus.Variant {
	p := map[string]dbus.Variant{}
	if it.Separator {
		p["type"] = dbus.MakeVariant("separator")
	} else {
		p["label"] = dbus.MakeVariant(it.Label)
		if it.Icon != "" {
			p["icon-name"] = dbus.MakeVariant(it.Icon)
		}
	}
	if it.Disabled {
		p["enabled"] = dbus.MakeVariant(false)
	}
	if len(it.Children) > 0 {
		p["children-display"] = dbus.MakeVariant("submenu")
	}
	if len(names) > 0 {
		for k := range p {
			keep := false
			for _, n := range names {
				if n == k {
					keep = true
				}
			}
			if !keep {
				delete(p, k)
			}
		}
	}
	return p
}

func layoutOf(it *MenuItem, depth int32, names []string) menuLayout {
	l := menuLayout{ID: it.ID, Props: itemProps(it, names), Children: []dbus.Variant{}}
	if depth != 0 {
		for _, c := range it.Children {
			l.Children = append(l.Children, dbus.MakeVariant(layoutOf(c, depth-1, names)))
		}
	}
	return l
}

// root is a synthetic item 0 holding the top-level entries.
func (m *dbusMenu) lookup(id int32) *MenuItem {
	if id == 0 {
		return &MenuItem{ID: 0, Children: m.items}
	}
	return m.idx[id]
}

func (m *dbusMenu) GetLayout(parent int32, depth int32, names []string) (uint32, menuLayout, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.lookup(parent)
	if it == nil {
		return m.rev, menuLayout{}, dbus.MakeFailedError(errNoItem)
	}
	return m.rev, layoutOf(it, depth, names), nil
}

func (m *dbusMenu) GetGroupProperties(ids []int32, names []string) ([]menuProps, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []menuProps{}
	for _, id := range ids {
		if it := m.lookup(id); it != nil {
			out = append(out, menuProps{id, itemProps(it, names)})
		}
	}
	return out, nil
}

func (m *dbusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it := m.lookup(id); it != nil {
		if v, ok := itemProps(it, nil)[name]; ok {
			return v, nil
		}
	}
	return dbus.Variant{}, dbus.MakeFailedError(errNoItem)
}

func (m *dbusMenu) event(id int32, eventID string) bool {
	if eventID != "clicked" {
		return true
	}
	m.mu.Lock()
	it := m.idx[id]
	m.mu.Unlock()
	if it == nil {
		return false
	}
	if it.Action != nil && !it.Disabled {
		go m.onAction(*it.Action)
	}
	return true
}

func (m *dbusMenu) Event(id int32, eventID string, data dbus.Variant, ts uint32) *dbus.Error {
	if !m.event(id, eventID) {
		return dbus.MakeFailedError(errNoItem)
	}
	return nil
}

func (m *dbusMenu) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	bad := []int32{}
	for _, e := range events {
		if !m.event(e.ID, e.EventID) {
			bad = append(bad, e.ID)
		}
	}
	return bad, nil
}

// AboutToShow refreshes the menu (the "checked 3 min ago" text ages). The
// change is announced with LayoutUpdated, so no extra update is requested.
func (m *dbusMenu) AboutToShow(id int32) (bool, *dbus.Error) {
	if m.onShow != nil {
		m.onShow()
	}
	return false, nil
}

func (m *dbusMenu) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}
