package fs

// End-to-end test of the D-Bus status/control service against the mock Graph
// server, on a private bus (the user's session bus is never touched).

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	onedriverdbus "github.com/jstaf/onedriver/fs/dbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startPrivateBus runs a private dbus-daemon and returns its address.
func startPrivateBus(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon is not installed")
	}
	cmd := exec.Command(path, "--session", "--print-address", "--nofork")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	addr, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSpace(addr)
}

func busConn(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestDBusService(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_dbus"
	if mock == nil {
		t.Skip("needs the mock Graph server")
	}
	addr := startPrivateBus(t)
	dir, cache := mountMockFS(t, remoteDir)
	mnt := strings.TrimSuffix(dir, remoteDir)

	svc, err := onedriverdbus.Start(busConn(t, addr), mnt, cache)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	assert.Equal(t, onedriverdbus.BusName(mnt), svc.Name())

	client := busConn(t, addr)
	obj := client.Object(svc.Name(), onedriverdbus.ObjectPath)
	call := func(method string, args ...interface{}) *dbus.Call {
		return obj.Call(onedriverdbus.Interface+"."+method, 0, args...)
	}
	getStatus := func() map[string]dbus.Variant {
		var st map[string]dbus.Variant
		require.NoError(t, call("GetStatus").Store(&st))
		return st
	}

	// signals
	require.NoError(t, client.AddMatchSignal(
		dbus.WithMatchInterface(onedriverdbus.Interface), dbus.WithMatchMember("Notify")))
	require.NoError(t, client.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged")))
	signals := make(chan *dbus.Signal, 64)
	client.Signal(signals)

	st := getStatus()
	assert.Equal(t, mnt, st["MountPoint"].Value())
	assert.Equal(t, "idle", st["State"].Value())
	assert.Equal(t, false, st["Paused"].Value())
	assert.NotEmpty(t, st["Account"].Value())
	// the quota arrives asynchronously
	assert.Eventually(t, func() bool {
		return getStatus()["QuotaTotal"].Value().(uint64) > 0
	}, 10*time.Second, 100*time.Millisecond)

	// the standard Properties interface works too
	var state dbus.Variant
	require.NoError(t, obj.Call("org.freedesktop.DBus.Properties.Get", 0, onedriverdbus.Interface, "State").Store(&state))
	assert.Equal(t, "idle", state.Value())

	// pause: writes work locally but are not uploaded
	require.NoError(t, call("Pause").Err)
	assert.Equal(t, "paused", getStatus()["State"].Value())
	fname := filepath.Join(dir, "held.txt")
	require.NoError(t, os.WriteFile(fname, []byte("held back"), 0644))
	assert.Eventually(t, func() bool {
		return getStatus()["PendingUploads"].Value().(uint32) > 0
	}, 5*time.Second, 50*time.Millisecond)
	time.Sleep(5 * time.Second) // several upload ticks
	_, uploaded := mock.ContentPath(remoteDir + "/held.txt")
	assert.False(t, uploaded, "upload must be held while paused")
	local, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "held back", string(local))

	// PropertiesChanged was emitted for the state change
	sawChange := false
drain:
	for {
		select {
		case sig := <-signals:
			if sig.Name == "org.freedesktop.DBus.Properties.PropertiesChanged" {
				changed := sig.Body[1].(map[string]dbus.Variant)
				if s, ok := changed["State"]; ok && s.Value() == "paused" {
					sawChange = true
				}
			}
		default:
			break drain
		}
	}
	assert.True(t, sawChange, "no PropertiesChanged for State")

	require.NoError(t, call("Resume").Err)
	assert.Eventually(t, func() bool {
		data, ok := mock.ContentPath(remoteDir + "/held.txt")
		return ok && string(data) == "held back"
	}, 30*time.Second, 200*time.Millisecond, "upload must proceed after resume")
	assert.Eventually(t, func() bool {
		return getStatus()["State"].Value() == "idle"
	}, 10*time.Second, 100*time.Millisecond)
	assert.NotZero(t, getStatus()["LastSync"].Value())

	// recent activity, newest first
	var recent []struct {
		Path   string
		Time   int64
		Action string
	}
	require.NoError(t, call("GetRecent", uint32(10)).Store(&recent))
	require.NotEmpty(t, recent)
	assert.Equal(t, "e2e_dbus/held.txt", recent[0].Path)
	assert.Equal(t, "uploaded", recent[0].Action)
	require.NoError(t, call("GetRecent", uint32(0)).Store(&recent))

	// file state, relative and absolute paths
	var fstate string
	var pinned bool
	require.NoError(t, call("GetFileState", "e2e_dbus/held.txt").Store(&fstate, &pinned))
	assert.Equal(t, "local", fstate)
	assert.False(t, pinned)
	require.NoError(t, call("SetPinned", fname, true).Err)
	require.NoError(t, call("GetFileState", fname).Store(&fstate, &pinned))
	assert.True(t, pinned)
	require.NoError(t, call("SetPinned", "e2e_dbus/held.txt", false).Err)
	require.NoError(t, call("GetFileState", fname).Store(&fstate, &pinned))
	assert.False(t, pinned)
	require.NoError(t, call("Evict", "e2e_dbus/held.txt").Err)
	require.NoError(t, call("GetFileState", fname).Store(&fstate, &pinned))
	assert.Equal(t, "cloud", fstate)

	// web URL
	var url string
	require.NoError(t, call("GetWebURL", fname).Store(&url))
	assert.Equal(t, "https://onedrive.mock.invalid/items/"+mock.ID(remoteDir+"/held.txt"), url)

	// errors
	err = call("GetFileState", "e2e_dbus/nope.txt").Err
	require.Error(t, err)
	assert.Equal(t, "org.freedesktop.DBus.Error.FileNotFound", err.(dbus.Error).Name)
	err = call("GetWebURL", "/somewhere/else").Err
	require.Error(t, err)
	assert.Equal(t, "org.freedesktop.DBus.Error.FileNotFound", err.(dbus.Error).Name)

	// conflict: Notify signal and "conflict" activity
	doc := filepath.Join(dir, "doc.txt")
	id := mock.PutFile(remoteDir+"/doc.txt", []byte("original"))
	require.Eventually(t, func() bool { _, err := os.Stat(doc); return err == nil }, 10*time.Second, 100*time.Millisecond)
	_, err = os.ReadFile(doc)
	require.NoError(t, err)
	mock.PauseUploads(remoteDir)
	defer mock.ResumeUploads(remoteDir)
	require.NoError(t, os.WriteFile(doc, []byte("local edit"), 0644))
	mock.Modify(id, []byte("remote edit"))

	var notify *dbus.Signal
	deadline := time.After(30 * time.Second)
wait:
	for {
		select {
		case sig := <-signals:
			if sig.Name == onedriverdbus.Interface+".Notify" {
				notify = sig
				break wait
			}
		case <-deadline:
			t.Fatal("no Notify signal for the conflict")
		}
	}
	assert.Equal(t, "conflict", notify.Body[0])
	assert.True(t, strings.HasPrefix(notify.Body[1].(string), "e2e_dbus/doc (conflict "), "%v", notify.Body[1])
	assert.NotEmpty(t, notify.Body[2])
	require.NoError(t, call("GetRecent", uint32(5)).Store(&recent))
	assert.Equal(t, "conflict", recent[0].Action)
}
