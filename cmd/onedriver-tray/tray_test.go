package main

import (
	"bufio"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startBus launches a private session bus; the real one is never touched.
func startBus(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command(path, "--session", "--print-address", "--nofork")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSpace(addr)
}

func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	c, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

// fakeDaemon implements the daemon side of the Mount1 interface.
type fakeDaemon struct {
	conn *dbus.Conn
	name string

	mu     sync.Mutex
	status map[string]dbus.Variant
	recent []fakeRecent
	calls  []string
}

type fakeRecent struct {
	Path   string
	Time   int64
	Action string
}

func newFakeDaemon(t *testing.T, addr, key, mount string) *fakeDaemon {
	d := &fakeDaemon{conn: connect(t, addr), name: mountPrefix + key}
	d.status = map[string]dbus.Variant{
		"MountPoint": dbus.MakeVariant(mount), "Account": dbus.MakeVariant("me@example.com"),
		"State": dbus.MakeVariant("idle"), "Paused": dbus.MakeVariant(false),
		"PendingUploads": dbus.MakeVariant(uint32(0)), "PendingDownloads": dbus.MakeVariant(uint32(0)),
		"QuotaUsed": dbus.MakeVariant(uint64(1 << 30)), "QuotaTotal": dbus.MakeVariant(uint64(5 << 30)),
		"LastSync": dbus.MakeVariant(int64(0)), "LastError": dbus.MakeVariant(""),
	}
	d.recent = []fakeRecent{{"a.txt", 100, "uploaded"}}
	require.NoError(t, d.conn.Export(d, mountObj, mountIface))
	return d
}

func (d *fakeDaemon) start(t *testing.T) {
	r, err := d.conn.RequestName(d.name, dbus.NameFlagDoNotQueue)
	require.NoError(t, err)
	require.Equal(t, dbus.RequestNameReplyPrimaryOwner, r)
}

func (d *fakeDaemon) stop(t *testing.T) {
	_, err := d.conn.ReleaseName(d.name)
	require.NoError(t, err)
}

func (d *fakeDaemon) set(k string, v interface{}) {
	d.mu.Lock()
	d.status[k] = dbus.MakeVariant(v)
	d.mu.Unlock()
	d.conn.Emit(mountObj, propsIface+".PropertiesChanged", mountIface,
		map[string]dbus.Variant{k: dbus.MakeVariant(v)}, []string{})
}

func (d *fakeDaemon) called() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *fakeDaemon) GetStatus() (map[string]dbus.Variant, *dbus.Error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]dbus.Variant{}
	for k, v := range d.status {
		out[k] = v
	}
	return out, nil
}

func (d *fakeDaemon) GetRecent(limit uint32) ([]fakeRecent, *dbus.Error) {
	return d.recent, nil
}

func (d *fakeDaemon) rec(m string) *dbus.Error {
	d.mu.Lock()
	d.calls = append(d.calls, m)
	d.mu.Unlock()
	return nil
}
func (d *fakeDaemon) Pause() *dbus.Error   { return d.rec("Pause") }
func (d *fakeDaemon) Resume() *dbus.Error  { return d.rec("Resume") }
func (d *fakeDaemon) SyncNow() *dbus.Error { return d.rec("SyncNow") }

// fakeNotifications is a stand-in for org.freedesktop.Notifications.
type fakeNotifications struct {
	got chan notifReq
}

type notifReq struct {
	Summary, Body string
	Actions       []string
}

func (f *fakeNotifications) Notify(app string, replaces uint32, icon, summary, body string,
	actions []string, hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	f.got <- notifReq{summary, body, actions}
	return 7, nil
}

type env struct {
	addr   string
	tray   *Tray
	client *dbus.Conn
	notifs *fakeNotifications
	ran    chan []string
	// installed makes LookPath find onedriver-activity
	installed atomic.Bool
}

// fakeActivity is the onedriver-activity side of Activity1.
type fakeActivity struct{ toggles chan struct{} }

func (f *fakeActivity) Toggle() *dbus.Error {
	f.toggles <- struct{}{}
	return nil
}

func newEnv(t *testing.T) *env {
	addr := startBus(t)
	e := &env{addr: addr, client: connect(t, addr), ran: make(chan []string, 16),
		notifs: &fakeNotifications{got: make(chan notifReq, 16)}}

	nc := connect(t, addr)
	require.NoError(t, nc.Export(e.notifs, notifObj, notifDst))
	_, err := nc.RequestName(notifDst, dbus.NameFlagDoNotQueue)
	require.NoError(t, err)

	e.tray, err = NewTray(connect(t, addr), Options{
		NotifyWindow: 150 * time.Millisecond,
		Run:          func(name string, args ...string) { e.ran <- append([]string{name}, args...) },
		LookPath: func(name string) (string, error) {
			if e.installed.Load() {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
	})
	require.NoError(t, err)
	go e.tray.Run()
	t.Cleanup(e.tray.Quit)
	// give Run time to install its match rules
	time.Sleep(200 * time.Millisecond)
	return e
}

func (e *env) sniProp(t *testing.T, name string) interface{} {
	v, err := e.client.Object(e.tray.sni.busName, sniPath).GetProperty(sniIface + "." + name)
	require.NoError(t, err)
	return v.Value()
}

type layoutNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

func (e *env) layout(t *testing.T) layoutNode {
	var rev uint32
	var l layoutNode
	err := e.client.Object(e.tray.sni.busName, menuPath).
		Call(menuIface+".GetLayout", 0, int32(0), int32(-1), []string{}).Store(&rev, &l)
	require.NoError(t, err)
	return l
}

func (n layoutNode) find(t *testing.T, label string) (int32, bool) {
	for _, c := range n.Children {
		var ch layoutNode
		require.NoError(t, dbus.Store([]interface{}{c.Value()}, &ch))
		if v, ok := ch.Props["label"]; ok && v.Value() == label {
			return ch.ID, true
		}
		if id, ok := ch.find(t, label); ok {
			return id, true
		}
	}
	return 0, false
}

func TestDiscoveryAndAggregation(t *testing.T) {
	e := newEnv(t)
	assert.Equal(t, "state-information", e.sniProp(t, "IconName"))
	_, ok := e.layout(t).find(t, "Open onedriver settings")
	assert.True(t, ok)

	d1 := newFakeDaemon(t, e.addr, "home_u_A", "/home/u/A")
	d1.start(t)
	assert.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "state-ok", e.sniProp(t, "IconName"))
	assert.Equal(t, "Active", e.sniProp(t, "Status"))

	d2 := newFakeDaemon(t, e.addr, "home_u_B", "/home/u/B")
	d2.set("State", "syncing")
	d2.start(t)
	assert.Eventually(t, func() bool { return len(e.tray.Instances()) == 2 }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "state-sync", e.sniProp(t, "IconName"))

	d1.set("State", "error")
	d1.set("LastError", "auth failed")
	assert.Eventually(t, func() bool { return e.sniProp(t, "Status") == "NeedsAttention" },
		3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "state-error", e.sniProp(t, "IconName"))

	d1.stop(t)
	assert.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)
	assert.Eventually(t, func() bool { return e.sniProp(t, "IconName") == "state-sync" },
		3*time.Second, 20*time.Millisecond)
	d2.stop(t)
	assert.Eventually(t, func() bool { return len(e.tray.Instances()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestMenuOverBusAndActions(t *testing.T) {
	e := newEnv(t)
	d := newFakeDaemon(t, e.addr, "home_u_A", "/home/u/A")
	d.start(t)
	require.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)

	l := e.layout(t)
	pause, ok := l.find(t, "Pause syncing")
	require.True(t, ok)
	_, ok = l.find(t, "a.txt (uploaded)")
	assert.True(t, ok)
	open, ok := l.find(t, "Open folder")
	require.True(t, ok)

	menu := e.client.Object(e.tray.sni.busName, menuPath)
	require.NoError(t, menu.Call(menuIface+".Event", 0, pause, "clicked", dbus.MakeVariant(""), uint32(0)).Err)
	assert.Eventually(t, func() bool { return len(d.called()) == 1 }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"Pause"}, d.called())

	require.NoError(t, menu.Call(menuIface+".Event", 0, open, "clicked", dbus.MakeVariant(""), uint32(0)).Err)
	select {
	case r := <-e.ran:
		assert.Equal(t, []string{"xdg-open", "/home/u/A"}, r)
	case <-time.After(3 * time.Second):
		t.Fatal("xdg-open was not run")
	}

	// left click opens the first instance's folder
	require.NoError(t, e.client.Object(e.tray.sni.busName, sniPath).Call(sniIface+".Activate", 0, int32(0), int32(0)).Err)
	select {
	case r := <-e.ran:
		assert.Equal(t, []string{"xdg-open", "/home/u/A"}, r)
	case <-time.After(3 * time.Second):
		t.Fatal("Activate did not open the folder")
	}

	// state change flips the menu entry
	d.set("State", "paused")
	d.set("Paused", true)
	assert.Eventually(t, func() bool { _, ok := e.layout(t).find(t, "Resume syncing"); return ok },
		3*time.Second, 20*time.Millisecond)
}

func TestActivityToggleAndStart(t *testing.T) {
	e := newEnv(t)
	d := newFakeDaemon(t, e.addr, "home_u_A", "/home/u/A")
	d.start(t)
	require.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)
	sniObj := e.client.Object(e.tray.sni.busName, sniPath)
	activate := func() {
		require.NoError(t, sniObj.Call(sniIface+".Activate", 0, int32(0), int32(0)).Err)
	}
	expectRun := func(want ...string) {
		select {
		case r := <-e.ran:
			assert.Equal(t, want, r)
		case <-time.After(3 * time.Second):
			t.Fatalf("expected %v to be run", want)
		}
	}

	// not running, not installed: falls back to the folder
	activate()
	expectRun("xdg-open", "/home/u/A")

	// not running, installed: started
	e.installed.Store(true)
	activate()
	expectRun("onedriver-activity")

	// the menu item does the same
	menu := e.client.Object(e.tray.sni.busName, menuPath)
	id, ok := e.layout(t).find(t, "Show activity")
	require.True(t, ok)
	require.NoError(t, menu.Call(menuIface+".Event", 0, id, "clicked", dbus.MakeVariant(""), uint32(0)).Err)
	expectRun("onedriver-activity")

	// running: Toggle is called, nothing is started
	act := &fakeActivity{toggles: make(chan struct{}, 4)}
	ac := connect(t, e.addr)
	require.NoError(t, ac.Export(act, activityObj, activityIface))
	_, err := ac.RequestName(activityName, dbus.NameFlagDoNotQueue)
	require.NoError(t, err)
	activate()
	select {
	case <-act.toggles:
	case <-time.After(3 * time.Second):
		t.Fatal("Toggle was not called")
	}
	require.NoError(t, menu.Call(menuIface+".Event", 0, id, "clicked", dbus.MakeVariant(""), uint32(0)).Err)
	select {
	case <-act.toggles:
	case <-time.After(3 * time.Second):
		t.Fatal("Toggle was not called from the menu")
	}
	select {
	case r := <-e.ran:
		t.Fatalf("unexpected run %v", r)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotifyBecomesNotification(t *testing.T) {
	e := newEnv(t)
	d := newFakeDaemon(t, e.addr, "home_u_A", "/home/u/A")
	d.start(t)
	require.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)

	d.conn.Emit(mountObj, mountIface+".Notify", "conflict", "x (conflicted).txt", "")
	select {
	case n := <-e.notifs.got:
		assert.Equal(t, "Conflict in x (conflicted).txt: kept both versions", n.Body)
	case <-time.After(3 * time.Second):
		t.Fatal("no notification")
	}

	// a burst is coalesced into one notification
	for _, p := range []string{"a", "b", "c", "d"} {
		d.conn.Emit(mountObj, mountIface+".Notify", "conflict", p, "")
	}
	select {
	case n := <-e.notifs.got:
		assert.Contains(t, n.Body, "4 conflicts")
	case <-time.After(3 * time.Second):
		t.Fatal("no summary notification")
	}
	select {
	case n := <-e.notifs.got:
		t.Fatalf("unexpected extra notification: %v", n)
	case <-time.After(400 * time.Millisecond):
	}

	d.conn.Emit(mountObj, mountIface+".Notify", "auth-expired", "", "")
	select {
	case n := <-e.notifs.got:
		assert.Equal(t, []string{"login", "Log in"}, n.Actions)
	case <-time.After(3 * time.Second):
		t.Fatal("no auth notification")
	}
	// clicking "Log in" runs the launcher. The fake service reports the
	// notification before the tray has stored its reply id, so keep clicking
	// until the tray knows about it (a real user clicks much later).
	deadline := time.After(3 * time.Second)
	for {
		require.NoError(t, e.client.Emit("/org/freedesktop/Notifications", notifDst+".ActionInvoked", uint32(7), "login"))
		select {
		case r := <-e.ran:
			assert.Equal(t, []string{"onedriver-launcher"}, r)
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("launcher not started")
		}
	}
}
