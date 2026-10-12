package fs

// End-to-end test of what happens when the login goes away while a filesystem
// is mounted: no window, no exit, cached reads and local writes keep working,
// and Login() over D-Bus brings everything back.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	onedriverdbus "github.com/jstaf/onedriver/fs/dbus"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/graph/mockgraph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthExpiredLoginE2E(t *testing.T) {
	// not parallel: it changes the token endpoint of the shared mock and the
	// process-wide login hook
	const remoteDir = "/e2e_auth_expired"
	if mock == nil {
		t.Skip("needs the mock Graph server")
	}
	defer mock.SetTokenMode(mockgraph.TokenOK)

	// the login window must never be opened by the filesystem
	var loginsN, failLogin int32
	restore := graph.SetInteractiveLogin(func(graph.AuthConfig, string) (string, error) {
		atomic.AddInt32(&loginsN, 1)
		if atomic.LoadInt32(&failLogin) != 0 {
			return "", errors.New("window closed")
		}
		return "fake-code", nil
	}, func() error { return nil })
	defer restore()
	logins := func() int { return int(atomic.LoadInt32(&loginsN)) }

	a := newMockAuth(mock)
	require.NoError(t, a.ToFile(filepath.Join(t.TempDir(), "auth_tokens.json")))

	mock.Mkdir(remoteDir)
	mock.PutFile(remoteDir+"/cached.txt", []byte("from the cloud"))
	cacheDir := filepath.Join(testDBLoc, "e2e_"+t.Name())
	mnt, cache, unmount := mountMockAuth(t, cacheDir, a, true)
	t.Cleanup(unmount)
	dir := filepath.Join(mnt, remoteDir)

	addr := startPrivateBus(t)
	svc, err := onedriverdbus.Start(busConn(t, addr), mnt, cache)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
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
	require.NoError(t, client.AddMatchSignal(
		dbus.WithMatchInterface(onedriverdbus.Interface), dbus.WithMatchMember("Notify")))
	signals := make(chan *dbus.Signal, 64)
	client.Signal(signals)
	authExpiredNotifications := func(wait time.Duration) int {
		n := 0
		deadline := time.After(wait)
		for {
			select {
			case sig := <-signals:
				if sig.Name == onedriverdbus.Interface+".Notify" && sig.Body[0] == "auth-expired" {
					n++
				}
			case <-deadline:
				return n
			}
		}
	}

	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "cached.txt")); return err == nil },
		10*time.Second, 100*time.Millisecond)
	data, err := os.ReadFile(filepath.Join(dir, "cached.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from the cloud", string(data))
	assert.Equal(t, false, getStatus()["AuthExpired"].Value())

	// 1. transient failure: nothing happens, everything keeps working
	mock.SetTokenMode(mockgraph.TokenUnavailable)
	a.SetExpiresAt(time.Now().Add(-time.Minute).Unix())
	require.Eventually(t, func() bool { return len(mock.TokenForms()) > 0 }, 10*time.Second, 100*time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "during_outage.txt"), []byte("outage"), 0644))
	require.Eventually(t, func() bool {
		d, ok := mock.ContentPath(remoteDir + "/during_outage.txt")
		return ok && string(d) == "outage"
	}, 30*time.Second, 200*time.Millisecond, "uploads continue with the old token")
	st := getStatus()
	assert.Equal(t, false, st["AuthExpired"].Value())
	assert.NotEqual(t, "error", st["State"].Value())
	// recovery: renewed once the backoff is over
	mock.SetTokenMode(mockgraph.TokenOK)
	require.Eventually(t, func() bool { return a.ExpiryTime() > time.Now().Unix() },
		30*time.Second, 200*time.Millisecond, "token renewed after recovery")
	assert.Equal(t, 0, authExpiredNotifications(time.Second), "transient failures are silent")
	assert.Equal(t, false, getStatus()["AuthExpired"].Value())

	// 2. the login is gone
	mock.SetTokenMode(mockgraph.TokenInvalidGrant)
	a.SetExpiresAt(time.Now().Add(-time.Minute).Unix())
	require.Eventually(t, func() bool {
		return getStatus()["AuthExpired"].Value() == true
	}, 30*time.Second, 100*time.Millisecond)
	st = getStatus()
	assert.Equal(t, "error", st["State"].Value())
	assert.Equal(t, "Login expired, click Log in", st["LastError"].Value())
	assert.Equal(t, 1, authExpiredNotifications(2*time.Second), "exactly one auth-expired notification")
	assert.Equal(t, 0, logins(), "no login window without a click")

	// cached files are still readable, changes are kept locally
	data, err = os.ReadFile(filepath.Join(dir, "cached.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from the cloud", string(data))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "offline.txt"), []byte("written while logged out"), 0644))
	data, err = os.ReadFile(filepath.Join(dir, "offline.txt"))
	require.NoError(t, err)
	assert.Equal(t, "written while logged out", string(data))
	time.Sleep(5 * time.Second) // several upload ticks
	_, uploaded := mock.ContentPath(remoteDir + "/offline.txt")
	assert.False(t, uploaded, "nothing is uploaded without a login")
	assert.Equal(t, true, getStatus()["AuthExpired"].Value())

	// 3. a failed login stays expired and notifies again
	atomic.StoreInt32(&failLogin, 1)
	require.NoError(t, call("Login").Err)
	assert.Equal(t, 1, authExpiredNotifications(5*time.Second), "failed login notifies again")
	assert.Equal(t, 1, logins())
	assert.Equal(t, true, getStatus()["AuthExpired"].Value())

	// 4. Login() restores everything
	atomic.StoreInt32(&failLogin, 0)
	require.NoError(t, call("Login").Err)
	require.Eventually(t, func() bool {
		return getStatus()["AuthExpired"].Value() == false
	}, 30*time.Second, 100*time.Millisecond)
	assert.Equal(t, 2, logins())
	require.Eventually(t, func() bool {
		d, ok := mock.ContentPath(remoteDir + "/offline.txt")
		return ok && string(d) == "written while logged out"
	}, 30*time.Second, 200*time.Millisecond, "the local change is uploaded after the login")
	require.Eventually(t, func() bool {
		st := getStatus()
		return st["State"].Value() != "error" && st["LastError"].Value() == ""
	}, 10*time.Second, 100*time.Millisecond)

	// sync works again
	mock.PutFile(remoteDir+"/after_login.txt", []byte("hello again"))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "after_login.txt"))
		return err == nil
	}, 30*time.Second, 200*time.Millisecond)
}

// without a display, Login() fails right away over D-Bus
func TestLoginHeadlessDBus(t *testing.T) {
	if mock == nil {
		t.Skip("needs the mock Graph server")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	addr := startPrivateBus(t)
	dir, cache := mountMockFS(t, "/e2e_login_headless")
	mnt := strings.TrimSuffix(dir, "/e2e_login_headless")
	svc, err := onedriverdbus.Start(busConn(t, addr), mnt, cache)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	obj := busConn(t, addr).Object(svc.Name(), onedriverdbus.ObjectPath)
	err = obj.Call(onedriverdbus.Interface+".Login", 0).Err
	require.Error(t, err)
	assert.Equal(t, "org.freedesktop.DBus.Error.Failed", err.(dbus.Error).Name)
	assert.Contains(t, err.Error(), "onedriver -n")
}
