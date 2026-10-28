// Package dbus exports the status and control interface of a mounted
// filesystem on the session bus, see docs/dbus-interface.md.
package dbus

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog/log"
)

// Backend is what the service needs from the filesystem. Paths are relative
// to the mountpoint. fs.Filesystem implements it. Subscribed handlers must not
// block.
type Backend interface {
	Status() status.Status
	Pause()
	Resume()
	SyncNow()
	// Resync starts over with a full enumeration of the drive, see
	// docs/dbus-interface.md
	Resync()
	// Login opens the login window (asynchronously), see docs/dbus-interface.md
	Login() error
	Recent(limit int) []status.Activity
	FileState(path string) (state string, pinned bool, err error)
	SetPinnedPath(path string, pinned bool) error
	EvictPath(path string) error
	WebURL(path string) (string, error)
	Transfers() []status.Transfer
	// TransfersVersion changes whenever Transfers may return something new
	TransfersVersion() uint64
	// TakeChangedPaths returns and clears the paths (relative to the
	// mountpoint) whose file state changed since the last call
	TakeChangedPaths() []string
	Subscribe(handler func(status.Notification)) (cancel func())
	// ErrNotFound reporting: errors for which IsNotFound is true map to
	// org.freedesktop.DBus.Error.FileNotFound
	IsNotFound(err error) bool
}

const (
	errFailed   = "org.freedesktop.DBus.Error.Failed"
	errNotFound = "org.freedesktop.DBus.Error.FileNotFound"
	propsIface  = "org.freedesktop.DBus.Properties"

	// PropertiesChanged is emitted at most this often
	changeInterval = time.Second

	// TransfersChanged is emitted at most this often
	transfersInterval = 250 * time.Millisecond

	// FileStatesChanged is emitted at most this often, with at most
	// maxStatePaths paths
	statesInterval = 250 * time.Millisecond
	maxStatePaths  = 500
)

// Service is the exported D-Bus object of one mount.
type Service struct {
	conn       *dbus.Conn
	mountpoint string
	backend    Backend
	name       string

	cancelSub func()
	notifyCh  chan status.Notification
	stop      chan struct{}
	done      sync.WaitGroup
	closeOnce sync.Once
}

// Connect connects to the session bus. The caller owns the connection.
func Connect() (*dbus.Conn, error) {
	return dbus.ConnectSessionBus()
}

// Start exports the interface for mountpoint (an absolute path) on conn and
// claims the bus name. Call Close to unexport.
func Start(conn *dbus.Conn, mountpoint string, backend Backend) (*Service, error) {
	s := &Service{
		conn:       conn,
		mountpoint: filepath.Clean(mountpoint),
		backend:    backend,
		name:       BusName(mountpoint),
		notifyCh:   make(chan status.Notification, 64),
		stop:       make(chan struct{}),
	}
	if err := conn.Export(&methods{s}, ObjectPath, Interface); err != nil {
		return nil, err
	}
	if err := conn.Export(&properties{s}, ObjectPath, propsIface); err != nil {
		return nil, err
	}
	if err := conn.Export(introspect.NewIntrospectable(introspectionNode()), ObjectPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	reply, err := conn.RequestName(s.name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, fmt.Errorf("bus name %s is already taken", s.name)
	}

	s.cancelSub = backend.Subscribe(func(n status.Notification) {
		select {
		case s.notifyCh <- n:
		default: // drop rather than block the filesystem
		}
	})
	s.done.Add(1)
	go s.loop()
	return s, nil
}

// Name returns the bus name that was claimed.
func (s *Service) Name() string { return s.name }

// Close stops emitting signals, unexports the object and releases the bus
// name. It does not close the connection.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.cancelSub()
		close(s.stop)
		s.done.Wait()
		s.conn.ReleaseName(s.name)
		s.conn.Export(nil, ObjectPath, Interface)
		s.conn.Export(nil, ObjectPath, propsIface)
		s.conn.Export(nil, ObjectPath, "org.freedesktop.DBus.Introspectable")
	})
}

// loop emits Notify signals and, at most once per changeInterval,
// PropertiesChanged with the properties that differ from the last emission.
func (s *Service) loop() {
	defer s.done.Done()
	ticker := time.NewTicker(changeInterval)
	defer ticker.Stop()
	transfersTicker := time.NewTicker(transfersInterval)
	defer transfersTicker.Stop()
	statesTicker := time.NewTicker(statesInterval)
	defer statesTicker.Stop()
	last := s.properties()
	lastTransfers := s.backend.TransfersVersion()
	for {
		select {
		case <-s.stop:
			return
		case <-transfersTicker.C:
			if v := s.backend.TransfersVersion(); v != lastTransfers {
				lastTransfers = v
				if err := s.conn.Emit(ObjectPath, Interface+".TransfersChanged"); err != nil {
					log.Debug().Err(err).Msg("Could not emit D-Bus TransfersChanged signal.")
				}
			}
		case <-statesTicker.C:
			paths := s.backend.TakeChangedPaths()
			if len(paths) == 0 {
				continue
			}
			paths = CollapsePaths(paths, maxStatePaths)
			if err := s.conn.Emit(ObjectPath, Interface+".FileStatesChanged", paths); err != nil {
				log.Debug().Err(err).Msg("Could not emit D-Bus FileStatesChanged signal.")
			}
		case n := <-s.notifyCh:
			err := s.conn.Emit(ObjectPath, Interface+".Notify", n.Kind, n.Path, n.Message)
			if err != nil {
				log.Debug().Err(err).Msg("Could not emit D-Bus Notify signal.")
			}
		case <-ticker.C:
			cur := s.properties()
			changed := make(map[string]dbus.Variant)
			for k, v := range cur {
				if old, ok := last[k]; !ok || !reflect.DeepEqual(old.Value(), v.Value()) {
					changed[k] = v
				}
			}
			last = cur
			if len(changed) == 0 {
				continue
			}
			err := s.conn.Emit(ObjectPath, propsIface+".PropertiesChanged",
				Interface, changed, []string{})
			if err != nil {
				log.Debug().Err(err).Msg("Could not emit D-Bus PropertiesChanged signal.")
			}
		}
	}
}

// CollapsePaths deduplicates paths and, if there are more than max, replaces
// them by their unique parent folders; if there are still more than max, by
// the mount root "".
func CollapsePaths(paths []string, max int) []string {
	uniq := func(in []string, f func(string) string) []string {
		seen := make(map[string]struct{}, len(in))
		out := make([]string, 0, len(in))
		for _, p := range in {
			if f != nil {
				p = f(p)
			}
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				out = append(out, p)
			}
		}
		return out
	}
	out := uniq(paths, nil)
	if len(out) <= max {
		return out
	}
	out = uniq(out, func(p string) string {
		d := filepath.Dir(p)
		if d == "." || d == "/" {
			return ""
		}
		return d
	})
	if len(out) <= max {
		return out
	}
	return []string{""}
}

// properties returns all properties as variants.
func (s *Service) properties() map[string]dbus.Variant {
	st := s.backend.Status()
	v := dbus.MakeVariant
	return map[string]dbus.Variant{
		"MountPoint":       v(s.mountpoint),
		"Account":          v(st.Account),
		"State":            v(st.State()),
		"Paused":           v(st.Paused),
		"PendingUploads":   v(st.PendingUploads),
		"PendingDownloads": v(st.PendingDownloads),
		"PinnedFiles":      v(st.PinnedFiles),
		"PinnedBytes":      v(st.PinnedBytes),
		"CacheBytes":       v(st.CacheBytes),
		"QuotaUsed":        v(st.QuotaUsed),
		"QuotaTotal":       v(st.QuotaTotal),
		"LastSync":         v(st.LastSync),
		"LastError":        v(st.LastError),
		"AuthExpired":      v(st.AuthExpired),
	}
}

// relative converts a path given by a client (absolute, or relative to the
// mountpoint) into a path relative to the mountpoint.
func (s *Service) relative(path string) (string, *dbus.Error) {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(s.mountpoint, filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", dbus.NewError(errNotFound, []interface{}{path + " is not inside " + s.mountpoint})
		}
		path = rel
	}
	path = filepath.Clean(path)
	if path == ".." || strings.HasPrefix(path, "../") {
		return "", dbus.NewError(errNotFound, []interface{}{path + " is outside the mountpoint"})
	}
	if path == "." {
		path = ""
	}
	return path, nil
}

func (s *Service) fail(err error) *dbus.Error {
	if err == nil {
		return nil
	}
	if s.backend.IsNotFound(err) {
		return dbus.NewError(errNotFound, []interface{}{err.Error()})
	}
	return dbus.NewError(errFailed, []interface{}{err.Error()})
}

// methods are the methods of io.github.onedriver.Mount1.
type methods struct{ s *Service }

func (m *methods) GetStatus() (map[string]dbus.Variant, *dbus.Error) {
	return m.s.properties(), nil
}

func (m *methods) Pause() *dbus.Error {
	m.s.backend.Pause()
	return nil
}

func (m *methods) Resume() *dbus.Error {
	m.s.backend.Resume()
	return nil
}

func (m *methods) SyncNow() *dbus.Error {
	m.s.backend.SyncNow()
	return nil
}

func (m *methods) Resync() *dbus.Error {
	m.s.backend.Resync()
	return nil
}

func (m *methods) Login() *dbus.Error {
	return m.s.fail(m.s.backend.Login())
}

// activity is one a(sxs) element of GetRecent.
type activity struct {
	Path   string
	Time   int64
	Action string
}

func (m *methods) GetRecent(limit uint32) ([]activity, *dbus.Error) {
	out := []activity{}
	for _, a := range m.s.backend.Recent(int(limit)) {
		out = append(out, activity{a.Path, a.Time, a.Action})
	}
	return out, nil
}

// transfer is one a(sssttx) element of GetTransfers.
type transfer struct {
	Path       string
	Direction  string
	State      string
	BytesDone  uint64
	BytesTotal uint64
	QueuedAt   int64
}

func (m *methods) GetTransfers() ([]transfer, *dbus.Error) {
	out := []transfer{}
	for _, t := range m.s.backend.Transfers() {
		out = append(out, transfer{t.Path, t.Direction, t.State, t.BytesDone, t.BytesTotal, t.QueuedAt})
	}
	return out, nil
}

func (m *methods) GetFileState(path string) (string, bool, *dbus.Error) {
	rel, derr := m.s.relative(path)
	if derr != nil {
		return "", false, derr
	}
	state, pinned, err := m.s.backend.FileState(rel)
	return state, pinned, m.s.fail(err)
}

func (m *methods) SetPinned(path string, pinned bool) *dbus.Error {
	rel, derr := m.s.relative(path)
	if derr != nil {
		return derr
	}
	return m.s.fail(m.s.backend.SetPinnedPath(rel, pinned))
}

func (m *methods) Evict(path string) *dbus.Error {
	rel, derr := m.s.relative(path)
	if derr != nil {
		return derr
	}
	return m.s.fail(m.s.backend.EvictPath(rel))
}

func (m *methods) GetWebURL(path string) (string, *dbus.Error) {
	rel, derr := m.s.relative(path)
	if derr != nil {
		return "", derr
	}
	url, err := m.s.backend.WebURL(rel)
	return url, m.s.fail(err)
}

// properties implements org.freedesktop.DBus.Properties (read-only).
type properties struct{ s *Service }

func (p *properties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface != Interface {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", []interface{}{iface})
	}
	v, ok := p.s.properties()[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []interface{}{name})
	}
	return v, nil
}

func (p *properties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != Interface {
		return map[string]dbus.Variant{}, nil
	}
	return p.s.properties(), nil
}

func (p *properties) Set(iface, name string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []interface{}{name})
}

func introspectionNode() *introspect.Node {
	prop := func(name, sig string) introspect.Property {
		return introspect.Property{Name: name, Type: sig, Access: "read"}
	}
	arg := func(name, sig, dir string) introspect.Arg {
		return introspect.Arg{Name: name, Type: sig, Direction: dir}
	}
	method := func(name string, args ...introspect.Arg) introspect.Method {
		return introspect.Method{Name: name, Args: args}
	}
	return &introspect.Node{
		Name: ObjectPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			{
				Name: propsIface,
				Methods: []introspect.Method{
					method("Get", arg("interface_name", "s", "in"), arg("property_name", "s", "in"), arg("value", "v", "out")),
					method("GetAll", arg("interface_name", "s", "in"), arg("props", "a{sv}", "out")),
					method("Set", arg("interface_name", "s", "in"), arg("property_name", "s", "in"), arg("value", "v", "in")),
				},
				Signals: []introspect.Signal{{Name: "PropertiesChanged", Args: []introspect.Arg{
					arg("interface_name", "s", ""), arg("changed_properties", "a{sv}", ""), arg("invalidated_properties", "as", ""),
				}}},
			},
			{
				Name: Interface,
				Methods: []introspect.Method{
					method("GetStatus", arg("status", "a{sv}", "out")),
					method("Pause"),
					method("Resume"),
					method("SyncNow"),
					method("Resync"),
					method("Login"),
					method("GetRecent", arg("limit", "u", "in"), arg("items", "a(sxs)", "out")),
					method("GetFileState", arg("path", "s", "in"), arg("state", "s", "out"), arg("pinned", "b", "out")),
					method("SetPinned", arg("path", "s", "in"), arg("pinned", "b", "in")),
					method("Evict", arg("path", "s", "in")),
					method("GetWebURL", arg("path", "s", "in"), arg("url", "s", "out")),
					method("GetTransfers", arg("transfers", "a(sssttx)", "out")),
				},
				Signals: []introspect.Signal{
					{Name: "Notify", Args: []introspect.Arg{
						arg("kind", "s", ""), arg("path", "s", ""), arg("message", "s", ""),
					}},
					{Name: "TransfersChanged"},
					{Name: "FileStatesChanged", Args: []introspect.Arg{arg("paths", "as", "")}},
				},
				Properties: []introspect.Property{
					prop("MountPoint", "s"), prop("Account", "s"), prop("State", "s"),
					prop("Paused", "b"), prop("PendingUploads", "u"), prop("PendingDownloads", "u"),
					prop("PinnedFiles", "u"), prop("PinnedBytes", "t"), prop("CacheBytes", "t"),
					prop("QuotaUsed", "t"), prop("QuotaTotal", "t"), prop("LastSync", "x"),
					prop("LastError", "s"), prop("AuthExpired", "b"),
				},
			},
		},
	}
}
