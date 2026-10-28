package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/rs/zerolog/log"
)

const (
	mountPrefix = "io.github.onedriver.Mount."
	mountObj    = dbus.ObjectPath("/io/github/onedriver/Mount")
	mountIface  = "io.github.onedriver.Mount1"
	propsIface  = "org.freedesktop.DBus.Properties"
	callTimeout = 3 * time.Second

	activityName  = "io.github.onedriver.Activity"
	activityObj   = dbus.ObjectPath("/io/github/onedriver/Activity")
	activityIface = "io.github.onedriver.Activity1"
	activityBin   = "onedriver-activity"
	recentLimit   = 10
)

// Options configures a Tray. The zero value is fine for production use.
type Options struct {
	// Run starts an external program (xdg-open, onedriver-launcher). Tests
	// replace it.
	Run func(name string, args ...string)
	// LookPath finds a program in $PATH (exec.LookPath by default). Tests
	// replace it.
	LookPath func(name string) (string, error)
	// NotifyWindow is how long Notify signals are collected before one
	// notification (or a summary) is shown.
	NotifyWindow time.Duration
}

// Tray discovers onedriver instances on the bus and drives the tray icon.
type Tray struct {
	conn *dbus.Conn
	opts Options

	sni   *sni
	menu  *dbusMenu
	notif *Notifier

	mu      sync.Mutex
	insts   map[string]*Instance // by bus name
	owners  map[string]string    // unique name -> bus name
	menuSig string

	sigs     chan *dbus.Signal
	quit     chan struct{}
	quitOnce sync.Once
}

// NewTray exports the tray objects on conn. It does not fail if no
// StatusNotifierWatcher is running (yet).
func NewTray(conn *dbus.Conn, opts Options) (*Tray, error) {
	if opts.Run == nil {
		opts.Run = runDetached
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.NotifyWindow == 0 {
		opts.NotifyWindow = 1500 * time.Millisecond
	}
	t := &Tray{
		conn:   conn,
		opts:   opts,
		insts:  map[string]*Instance{},
		owners: map[string]string{},
		sigs:   make(chan *dbus.Signal, 128),
		quit:   make(chan struct{}),
	}
	var err error
	if t.menu, err = newDBusMenu(conn, t.Do, t.rebuild); err != nil {
		return nil, err
	}
	if t.sni, err = newSNI(conn, t.activate); err != nil {
		return nil, err
	}
	t.notif = newNotifier(conn, opts.NotifyWindow, func(inst string) { t.Do(Action{Kind: ActLogin, Instance: inst}) })
	t.rebuild()
	return t, nil
}

// Quit makes Run return.
func (t *Tray) Quit() { t.quitOnce.Do(func() { close(t.quit) }) }

// Done is closed once Quit was called.
func (t *Tray) Done() <-chan struct{} { return t.quit }

// Instances returns a snapshot of all known instances.
func (t *Tray) Instances() []Instance {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Instance, 0, len(t.insts))
	for _, i := range t.insts {
		out = append(out, *i)
	}
	return sorted(out)
}

// Run subscribes to the bus and processes signals until Quit is called.
func (t *Tray) Run() error {
	t.conn.Signal(t.sigs)
	defer t.conn.RemoveSignal(t.sigs)

	matches := [][]dbus.MatchOption{
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
			dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg0Namespace("io.github.onedriver.Mount")},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
			dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg(0, watcherDst)},
		{dbus.WithMatchObjectPath(mountObj), dbus.WithMatchInterface(propsIface),
			dbus.WithMatchMember("PropertiesChanged")},
		{dbus.WithMatchObjectPath(mountObj), dbus.WithMatchInterface(mountIface),
			dbus.WithMatchMember("Notify")},
		{dbus.WithMatchInterface(notifDst), dbus.WithMatchMember("ActionInvoked")},
	}
	for _, m := range matches {
		if err := t.conn.AddMatchSignal(m...); err != nil {
			return err
		}
	}

	if err := t.sni.Register(); err != nil {
		log.Warn().Err(err).Msg("No StatusNotifierWatcher yet, will register when it appears.")
	}

	var names []string
	if err := t.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return err
	}
	for _, n := range names {
		if len(n) > len(mountPrefix) && n[:len(mountPrefix)] == mountPrefix {
			t.refresh(n)
		}
	}

	// the "checked 3 min ago" texts age without any signal
	refresh := time.NewTicker(30 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-t.quit:
			return nil
		case <-refresh.C:
			t.rebuild()
		case sig, ok := <-t.sigs:
			if !ok || sig == nil {
				// godbus closes the channel when the bus connection goes away
				return errors.New("session bus connection closed")
			}
			t.handle(sig)
		}
	}
}

func (t *Tray) handle(sig *dbus.Signal) {
	switch sig.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(sig.Body) < 3 {
			return
		}
		name, _ := sig.Body[0].(string)
		newOwner, _ := sig.Body[2].(string)
		switch {
		case name == watcherDst:
			if newOwner != "" {
				time.Sleep(200 * time.Millisecond)
				if err := t.sni.Register(); err != nil {
					log.Warn().Err(err).Msg("Could not register with StatusNotifierWatcher.")
				}
			}
		case len(name) > len(mountPrefix) && name[:len(mountPrefix)] == mountPrefix:
			if newOwner == "" {
				t.remove(name)
			} else {
				t.refresh(name)
			}
		}
	case propsIface + ".PropertiesChanged":
		if name, ok := t.busNameOf(sig.Sender); ok {
			t.refresh(name)
		}
	case mountIface + ".Notify":
		name, ok := t.busNameOf(sig.Sender)
		if !ok || len(sig.Body) < 3 {
			return
		}
		kind, _ := sig.Body[0].(string)
		p, _ := sig.Body[1].(string)
		msg, _ := sig.Body[2].(string)
		mount := name
		t.mu.Lock()
		if i := t.insts[name]; i != nil {
			mount = i.MountPoint
		}
		t.mu.Unlock()
		t.notif.Add(notifEvent{Kind: kind, Path: p, Message: msg, Mount: mount, Instance: name})
	case notifDst + ".ActionInvoked":
		if len(sig.Body) >= 2 {
			id, _ := sig.Body[0].(uint32)
			key, _ := sig.Body[1].(string)
			t.notif.ActionInvoked(id, key)
		}
	}
}

func (t *Tray) busNameOf(sender string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.owners[sender]
	return n, ok
}

func (t *Tray) remove(name string) {
	t.mu.Lock()
	delete(t.insts, name)
	for o, n := range t.owners {
		if n == name {
			delete(t.owners, o)
		}
	}
	t.mu.Unlock()
	t.rebuild()
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

// refresh (re)reads status and recent files of one instance.
func (t *Tray) refresh(name string) {
	c, cancel := ctx()
	defer cancel()
	var owner string
	if err := t.conn.BusObject().CallWithContext(c, "org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner); err != nil {
		return
	}
	obj := t.conn.Object(name, mountObj)
	var st map[string]dbus.Variant
	if err := obj.CallWithContext(c, mountIface+".GetStatus", 0).Store(&st); err != nil {
		log.Warn().Err(err).Str("name", name).Msg("GetStatus failed.")
		return
	}
	inst := parseStatus(name, st)
	var recent []struct {
		Path   string
		Time   int64
		Action string
	}
	if err := obj.CallWithContext(c, mountIface+".GetRecent", 0, uint32(recentLimit)).Store(&recent); err == nil {
		for _, r := range recent {
			inst.Recent = append(inst.Recent, RecentItem{r.Path, r.Time, r.Action})
		}
	}
	t.mu.Lock()
	t.insts[name] = &inst
	t.owners[owner] = name
	t.mu.Unlock()
	t.rebuild()
}

func parseStatus(name string, st map[string]dbus.Variant) Instance {
	str := func(k string) string { s, _ := st[k].Value().(string); return s }
	u32 := func(k string) uint32 { u, _ := st[k].Value().(uint32); return u }
	u64 := func(k string) uint64 { u, _ := st[k].Value().(uint64); return u }
	b := func(k string) bool { v, _ := st[k].Value().(bool); return v }
	var ls int64
	ls, _ = st["LastSync"].Value().(int64)
	// missing on daemons older than interface v1.5: false
	checking := b("Checking")
	return Instance{
		BusName: name, MountPoint: str("MountPoint"), Account: str("Account"),
		State: str("State"), Paused: b("Paused"),
		PendingUploads: u32("PendingUploads"), PendingDownloads: u32("PendingDownloads"),
		QuotaUsed: u64("QuotaUsed"), QuotaTotal: u64("QuotaTotal"),
		LastSync: ls, Checking: checking, LastError: str("LastError"), AuthExpired: b("AuthExpired"),
	}
}

func menuSignature(items []*MenuItem) string {
	s := ""
	for _, it := range items {
		a := ""
		if it.Action != nil {
			a = fmt.Sprint(*it.Action)
		}
		s += fmt.Sprintf("[%q %v %v %s %s]", it.Label, it.Separator, it.Disabled, it.Icon, a)
		if len(it.Children) > 0 {
			s += "{" + menuSignature(it.Children) + "}"
		}
	}
	return s
}

// rebuild recomputes icon, tooltip and menu from the current instances.
func (t *Tray) rebuild() bool {
	insts := t.Instances()
	state := AggregateState(insts)
	now := time.Now()
	title, desc := Tooltip(insts, now)
	t.sni.Update(state, title, desc)
	items := BuildMenuAt(insts, now)
	sig := menuSignature(items)
	t.mu.Lock()
	changed := sig != t.menuSig
	t.menuSig = sig
	t.mu.Unlock()
	if changed {
		t.menu.SetItems(items)
	}
	return changed
}

// activate is the left click: toggle the activity window, starting
// onedriver-activity if it is not running, and fall back to opening the first
// instance's folder if it is not installed.
func (t *Tray) activate(x, y int32) {
	if t.showActivity(x, y) {
		return
	}
	t.activateFallback()
}

// activateFallback opens the first instance's folder, or the settings if
// nothing is running.
func (t *Tray) activateFallback() {
	insts := t.Instances()
	if len(insts) == 0 {
		t.Do(Action{Kind: ActSettings})
		return
	}
	t.Do(Action{Kind: ActOpenFolder, Arg: insts[0].MountPoint})
}

// showActivity toggles the activity window, opening it next to (x, y), the
// global position of the click on the tray icon. Hosts that do not report the
// position send (0, 0), which opens the window in its default corner. It
// returns false if there is no activity window to show (the program is not
// installed or could not be reached).
func (t *Tray) showActivity(x, y int32) bool {
	positioned := x != 0 || y != 0
	c, cancel := ctx()
	defer cancel()
	var hasOwner bool
	err := t.conn.BusObject().CallWithContext(c, "org.freedesktop.DBus.NameHasOwner", 0, activityName).Store(&hasOwner)
	if err == nil && hasOwner {
		obj := t.conn.Object(activityName, activityObj)
		if positioned {
			err = obj.CallWithContext(c, activityIface+".ToggleAt", 0, x, y).Err
			if isUnknownMethod(err) { // an activity window from before ToggleAt
				err = obj.CallWithContext(c, activityIface+".Toggle", 0).Err
			}
		} else {
			err = obj.CallWithContext(c, activityIface+".Toggle", 0).Err
		}
		if err != nil {
			log.Warn().Err(err).Msg("Could not toggle the activity window.")
			return false
		}
		return true
	}
	if _, err := t.opts.LookPath(activityBin); err != nil {
		return false
	}
	// without --show onedriver-activity starts hidden
	if positioned {
		t.opts.Run(activityBin, "--show", fmt.Sprintf("--at=%d,%d", x, y))
	} else {
		t.opts.Run(activityBin, "--show")
	}
	return true
}

// login asks the daemon to open its login window. Daemons that do not have the
// Login method (older than interface v1.2) are handled by the launcher.
func (t *Tray) login(instance string) {
	c, cancel := ctx()
	defer cancel()
	err := t.conn.Object(instance, mountObj).CallWithContext(c, mountIface+".Login", 0).Err
	if err == nil {
		return
	}
	if isUnknownMethod(err) {
		t.opts.Run("onedriver-launcher")
		return
	}
	log.Warn().Err(err).Msg("Login() failed.")
	msg := err.Error()
	var derr dbus.Error
	if errors.As(err, &derr) && len(derr.Body) > 0 {
		if s, ok := derr.Body[0].(string); ok {
			msg = s
		}
	}
	t.notif.send(notification{Summary: "OneDrive", Body: "Could not open the login: " + msg, Icon: "dialog-error"})
}

// Do performs a menu action.
func (t *Tray) Do(a Action) {
	switch a.Kind {
	case ActOpenFolder, ActOpenFile:
		t.opts.Run("xdg-open", a.Arg)
	case ActSettings:
		t.opts.Run("onedriver-launcher")
	case ActActivity:
		if !t.showActivity(0, 0) {
			t.activateFallback()
		}
	case ActQuit:
		t.Quit()
	case ActLogin:
		t.login(a.Instance)
	case ActResync:
		c, cancel := ctx()
		defer cancel()
		err := t.conn.Object(a.Instance, mountObj).CallWithContext(c, mountIface+".Resync", 0).Err
		switch {
		case err == nil:
			t.notif.send(notification{Summary: "OneDrive", Body: "Refreshing everything from OneDrive...", Icon: "view-refresh"})
		case isUnknownMethod(err):
			t.notif.send(notification{Summary: "OneDrive", Body: "Not supported by this onedriver version", Icon: "dialog-information"})
		default:
			log.Warn().Err(err).Msg("Resync() failed.")
			t.notif.send(notification{Summary: "OneDrive", Body: "Could not refresh: " + err.Error(), Icon: "dialog-error"})
		}
	case ActPause, ActResume, ActSyncNow:
		method := map[string]string{ActPause: "Pause", ActResume: "Resume", ActSyncNow: "SyncNow"}[a.Kind]
		c, cancel := ctx()
		defer cancel()
		if err := t.conn.Object(a.Instance, mountObj).CallWithContext(c, mountIface+"."+method, 0).Err; err != nil {
			log.Warn().Err(err).Str("method", method).Msg("D-Bus call failed.")
		}
	}
}

// isUnknownMethod reports whether err says the called object or method does
// not exist, which is what an older version of the callee answers.
func isUnknownMethod(err error) bool {
	var derr dbus.Error
	return errors.As(err, &derr) && (derr.Name == "org.freedesktop.DBus.Error.UnknownMethod" ||
		derr.Name == "org.freedesktop.DBus.Error.UnknownObject")
}
