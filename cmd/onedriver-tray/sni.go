package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// StatusNotifierItem (the "tray icon" protocol used by KDE Plasma and most
// other desktops) exported directly with godbus.

const (
	sniPath    = dbus.ObjectPath("/StatusNotifierItem")
	sniIface   = "org.kde.StatusNotifierItem"
	watcherDst = "org.kde.StatusNotifierWatcher"
	watcherObj = dbus.ObjectPath("/StatusNotifierWatcher")
)

var errNoItem = errors.New("no such menu item")

type sniPixmap struct {
	W, H int32
	Data []byte
}

type sniToolTip struct {
	IconName string
	Pixmaps  []sniPixmap
	Title    string
	Desc     string
}

type sni struct {
	conn       *dbus.Conn
	props      *prop.Properties
	busName    string
	onActivate func(x, y int32)
}

func newSNI(conn *dbus.Conn, onActivate func(x, y int32)) (*sni, error) {
	s := &sni{conn: conn, onActivate: onActivate}
	s.busName = fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(s.busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, fmt.Errorf("could not own %s", s.busName)
	}
	if err := conn.Export(s, sniPath, sniIface); err != nil {
		return nil, err
	}
	empty := []sniPixmap{}
	props, err := prop.Export(conn, sniPath, map[string]map[string]*prop.Prop{sniIface: {
		"Category":            {Value: "ApplicationStatus"},
		"Id":                  {Value: "onedriver-tray"},
		"Title":               {Value: "OneDrive"},
		"Status":              {Value: "Active"},
		"WindowId":            {Value: uint32(0)},
		"IconName":            {Value: IconName(StateNone)},
		"IconPixmap":          {Value: empty},
		"OverlayIconName":     {Value: ""},
		"OverlayIconPixmap":   {Value: empty},
		"AttentionIconName":   {Value: "state-error"},
		"AttentionIconPixmap": {Value: empty},
		"AttentionMovieName":  {Value: ""},
		"ToolTip":             {Value: sniToolTip{"", empty, "OneDrive", ""}},
		"ItemIsMenu":          {Value: false},
		"Menu":                {Value: menuPath},
	}})
	if err != nil {
		return nil, err
	}
	s.props = props
	node := &introspect.Node{Name: string(sniPath), Interfaces: []introspect.Interface{
		introspect.IntrospectData, prop.IntrospectData,
		{Name: sniIface, Methods: introspect.Methods(s), Properties: props.Introspection(sniIface)},
	}}
	err = conn.Export(introspect.NewIntrospectable(node), sniPath, "org.freedesktop.DBus.Introspectable")
	return s, err
}

// Update pushes icon, status and tooltip and emits the change signals.
func (s *sni) Update(state, title, desc string) {
	set := func(name string, v interface{}) {
		if old, err := s.props.Get(sniIface, name); err == nil && old.Value() == v {
			return
		}
		s.props.SetMust(sniIface, name, v)
		switch name {
		case "IconName":
			s.conn.Emit(sniPath, sniIface+".NewIcon")
		case "Status":
			s.conn.Emit(sniPath, sniIface+".NewStatus", v)
		}
	}
	set("IconName", IconName(state))
	set("Status", SNIStatus(state))
	s.props.SetMust(sniIface, "ToolTip", sniToolTip{"", []sniPixmap{}, title, desc})
	s.conn.Emit(sniPath, sniIface+".NewToolTip")
}

// Register announces the item to the StatusNotifierWatcher, if there is one.
func (s *sni) Register() error {
	return s.conn.Object(watcherDst, watcherObj).
		Call(watcherDst+".RegisterStatusNotifierItem", 0, s.busName).Err
}

// WatchWatcher re-registers whenever the watcher (re)appears, e.g. after a
// plasmashell restart, or when it starts after us.
func (s *sni) WatchWatcher(ch <-chan *dbus.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case sig, ok := <-ch:
			if !ok {
				return
			}
			if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
				continue
			}
			name, _ := sig.Body[0].(string)
			owner, _ := sig.Body[2].(string)
			if name == watcherDst && owner != "" {
				time.Sleep(200 * time.Millisecond)
				_ = s.Register()
			}
		}
	}
}

func (s *sni) Activate(x, y int32) *dbus.Error          { go s.onActivate(x, y); return nil }
func (s *sni) SecondaryActivate(x, y int32) *dbus.Error { return nil }
func (s *sni) ContextMenu(x, y int32) *dbus.Error       { return nil }
func (s *sni) Scroll(delta int32, orientation string) *dbus.Error {
	return nil
}
