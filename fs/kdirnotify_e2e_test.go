package fs

// End-to-end test of the KDE KDirNotify signals for server-side changes, on a
// private bus (the user's session bus is never touched).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	onedriverdbus "github.com/jstaf/onedriver/fs/dbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKDirNotifyRemoteChanges(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_kdirnotify"
	if mock == nil {
		t.Skip("needs the mock Graph server")
	}
	addr := startPrivateBus(t)
	dir, cache := mountMockFS(t, remoteDir)
	mnt := strings.TrimSuffix(dir, remoteDir)

	svc, err := onedriverdbus.Start(busConn(t, addr), mnt, cache)
	require.NoError(t, err)
	t.Cleanup(svc.Close)

	client := busConn(t, addr)
	require.NoError(t, client.AddMatchSignal(dbus.WithMatchInterface("org.kde.KDirNotify")))
	signals := make(chan *dbus.Signal, 64)
	client.Signal(signals)

	url := func(rel string) string { return onedriverdbus.FileURL(mnt, rel) }

	// waits for the signal with this member
	next := func(member string) *dbus.Signal {
		t.Helper()
		timeout := time.After(15 * time.Second)
		for {
			select {
			case s := <-signals:
				if s.Name == "org.kde.KDirNotify."+member {
					assert.Equal(t, dbus.ObjectPath("/"), s.Path)
					return s
				}
			case <-timeout:
				require.FailNow(t, "no "+member+" signal")
			}
		}
	}

	// the folder has to be listed, a file manager would be showing it
	_, err = os.ReadDir(dir)
	require.NoError(t, err)
	time.Sleep(time.Second) // let the service start recording

	// added on the server
	id := mock.PutFile(remoteDir+"/a b.txt", []byte("one"))
	s := next("FilesAdded")
	assert.Equal(t, []interface{}{url("e2e_kdirnotify")}, s.Body)
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "a b.txt"))
		return err == nil
	}, 10*time.Second, 100*time.Millisecond)

	// overwritten on the server
	mock.Modify(id, []byte("two, longer"))
	s = next("FilesChanged")
	assert.Equal(t, []interface{}{[]string{url("e2e_kdirnotify/a b.txt")}}, s.Body)
	assert.Contains(t, url("e2e_kdirnotify/a b.txt"), "a%20b.txt")

	// renamed on the server
	require.NoError(t, mock.Move(id, "c.txt", mock.ID(remoteDir)))
	s = next("FileRenamed")
	assert.Equal(t, []interface{}{url("e2e_kdirnotify/a b.txt"), url("e2e_kdirnotify/c.txt")}, s.Body)
	s = next("FileRenamedWithLocalPath")
	assert.Equal(t, []interface{}{url("e2e_kdirnotify/a b.txt"), url("e2e_kdirnotify/c.txt"), ""}, s.Body)

	// deleted on the server
	mock.Delete(id)
	s = next("FilesRemoved")
	assert.Equal(t, []interface{}{[]string{url("e2e_kdirnotify/c.txt")}}, s.Body)

	// changes made through the mount are not announced, neither when they
	// are made nor when they come back as deltas after the upload
	for len(signals) > 0 {
		<-signals
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "local.txt"), []byte("mine"), 0644))
	require.NoError(t, os.Rename(filepath.Join(dir, "local.txt"), filepath.Join(dir, "local2.txt")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0755))
	require.Eventually(t, func() bool {
		_, ok := mock.Content(mock.ID(remoteDir + "/local2.txt"))
		return ok
	}, 20*time.Second, 100*time.Millisecond)
	time.Sleep(3 * time.Second) // several delta polls and flushes
	select {
	case s := <-signals:
		assert.Fail(t, "unexpected signal for a local change", "%s %v", s.Name, s.Body)
	default:
	}
}
