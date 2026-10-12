package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	onedriverdbus "github.com/jstaf/onedriver/fs/dbus"
	"github.com/stretchr/testify/require"
)

// TestDBusFileStatesChanged checks the FileStatesChanged signal for a
// download, a local write, a pin and an eviction.
func TestDBusFileStatesChanged(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_dbus_states"
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
	obj := client.Object(svc.Name(), onedriverdbus.ObjectPath)
	require.NoError(t, client.AddMatchSignal(
		dbus.WithMatchInterface(onedriverdbus.Interface), dbus.WithMatchMember("FileStatesChanged")))
	signals := make(chan *dbus.Signal, 256)
	client.Signal(signals)

	// expect waits for a signal that contains path
	expect := func(path string, what string) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			select {
			case sig := <-signals:
				if sig.Name != onedriverdbus.Interface+".FileStatesChanged" {
					continue
				}
				paths := sig.Body[0].([]string)
				require.LessOrEqual(t, len(paths), 500)
				for _, p := range paths {
					if p == path {
						return
					}
				}
			case <-deadline:
				t.Fatalf("no FileStatesChanged for %s (%s)", path, what)
			}
		}
	}
	drain := func() {
		time.Sleep(600 * time.Millisecond)
		for {
			select {
			case <-signals:
			default:
				return
			}
		}
	}

	mock.PutFile(remoteDir+"/cloud.txt", []byte("from the cloud"))
	mock.PutFile(remoteDir+"/sub/inner.txt", []byte("inner"))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "cloud.txt"))
		return err == nil
	}, 20*time.Second, 100*time.Millisecond)
	_, err = os.ReadDir(filepath.Join(dir, "sub")) // load the children
	require.NoError(t, err)
	time.Sleep(time.Second) // recording starts with the first drain
	drain()

	// download
	_, err = os.ReadFile(filepath.Join(dir, "cloud.txt"))
	require.NoError(t, err)
	expect("e2e_dbus_states/cloud.txt", "download")

	// local write
	drain()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0644))
	expect("e2e_dbus_states/new.txt", "write")

	// pin a folder: the folder and its loaded children
	drain()
	require.NoError(t, obj.Call(onedriverdbus.Interface+".SetPinned", 0, "e2e_dbus_states/sub", true).Err)
	expect("e2e_dbus_states/sub", "pin")

	// evict
	drain()
	require.NoError(t, obj.Call(onedriverdbus.Interface+".Evict", 0, "e2e_dbus_states/cloud.txt").Err)
	expect("e2e_dbus_states/cloud.txt", "evict")
}
