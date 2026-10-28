package fs

// Tests for applying deltas in any order (Graph does not list a folder before
// its children), for folders whose children could not be fetched, and for the
// Resync D-Bus method. They need the mock Graph server.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	onedriverdbus "github.com/jstaf/onedriver/fs/dbus"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountMockManual mounts without a running delta loop, so that the test decides
// when (and with which kind of delta link) polls happen.
func mountMockManual(t *testing.T, remoteDir string) (string, *Filesystem) {
	t.Helper()
	if mock == nil {
		t.Skip("needs the mock Graph server (unset ONEDRIVER_TEST_LIVE)")
	}
	mock.Mkdir(remoteDir)
	cacheDir := filepath.Join(testDBLoc, "e2e_"+strings.ReplaceAll(t.Name(), "/", "_"))
	mnt, cache, unmount := mountMockAtOpts(t, cacheDir, false)
	t.Cleanup(unmount)
	return filepath.Join(mnt, remoteDir), cache
}

func noteNames(n int) []string {
	names := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("Notes_%02d", i))
	}
	return names
}

// runOnePoll fetches all pages from the current delta link and applies them the
// way the delta loop does.
func runOnePoll(t *testing.T, cache *Filesystem) int {
	t.Helper()
	deltas := map[string]*graph.DriveItem{}
	pages := 0
	for {
		incoming, cont, err := cache.pollDeltas(auth)
		require.NoError(t, err)
		pages++
		for _, d := range incoming {
			deltas[d.ID] = d
		}
		if !cont {
			break
		}
	}
	cache.applyDeltas(deltas)
	return pages
}

// A new folder with files: the files are delivered before the folder, on one
// page or spread over several. Everything must be there afterwards, and the
// folder must not be left with a partial list.
func TestDeltaChildrenBeforeNewFolder(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		pages        int
	}{
		{"samePage", "&order=reverse", 1},
		{"splitPages", "&order=reverse&page=4", 3},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			remoteDir := "/e2e_order_" + tc.name
			dir, cache := mountMockManual(t, remoteDir)
			require.Empty(t, dirNames(t, dir))

			mock.Mkdir(remoteDir + "/Litterature")
			for _, n := range noteNames(10) {
				mock.PutFile(remoteDir+"/Litterature/"+n, []byte(n))
			}
			cache.deltaLink += tc.params
			pages := runOnePoll(t, cache)
			assert.GreaterOrEqual(t, pages, tc.pages)

			assert.ElementsMatch(t, noteNames(10), dirNames(t, filepath.Join(dir, "Litterature")))
		})
	}
}

// An item that moved into a folder that is new to us arrives before that
// folder: it must leave its old folder, and show up in the new one.
func TestDeltaMoveIntoNewFolderListedFirst(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_order_move"
	mock.Mkdir(remoteDir + "/old")
	moved := mock.PutFile(remoteDir+"/old/moved.txt", []byte("m"))
	mock.PutFile(remoteDir+"/old/stays.txt", []byte("s"))
	dir, cache := mountMockManual(t, remoteDir)
	require.ElementsMatch(t, []string{"moved.txt", "stays.txt"}, dirNames(t, filepath.Join(dir, "old")))

	newDir := mock.Mkdir(remoteDir + "/new")
	mock.PutFile(remoteDir+"/new/other.txt", []byte("o"))
	require.NoError(t, mock.Move(moved, "", newDir))
	cache.deltaLink += "&order=reverse"
	runOnePoll(t, cache)

	assert.Equal(t, []string{"stays.txt"}, dirNames(t, filepath.Join(dir, "old")))
	assert.ElementsMatch(t, []string{"moved.txt", "other.txt"}, dirNames(t, filepath.Join(dir, "new")))
}

// Deletions work whether the children or the folder come first.
func TestDeltaDeletionOrder(t *testing.T) {
	for _, params := range []string{"", "&order=reverse", "&order=reverse&page=2"} {
		params := params
		t.Run("order"+params, func(t *testing.T) {
			t.Parallel()
			remoteDir := fmt.Sprintf("/e2e_order_del%d", len(params))
			dir, cache := mountMockManual(t, remoteDir)
			mock.Mkdir(remoteDir + "/gone/inner")
			mock.PutFile(remoteDir+"/gone/a.txt", []byte("a"))
			mock.PutFile(remoteDir+"/gone/inner/b.txt", []byte("b"))
			mock.PutFile(remoteDir+"/keep.txt", []byte("k"))
			// load the whole tree
			cache.deltaLink += "&order=reverse"
			runOnePoll(t, cache)
			require.ElementsMatch(t, []string{"gone", "keep.txt"}, dirNames(t, dir))
			require.Equal(t, []string{"b.txt"}, dirNames(t, filepath.Join(dir, "gone", "inner")))
			goneID := mock.ID(remoteDir + "/gone")
			bID := mock.ID(remoteDir + "/gone/inner/b.txt")
			require.NotNil(t, cache.GetID(bID))

			mock.Delete(goneID)
			cache.deltaLink = fmt.Sprintf("%s%s", cache.deltaLink, params)
			runOnePoll(t, cache)

			assert.Equal(t, []string{"keep.txt"}, dirNames(t, dir))
			assert.Nil(t, cache.GetID(goneID))
			assert.Nil(t, cache.GetID(bID))
		})
	}
}

// A folder whose children could not be fetched (offline, no login) must not
// stay cached as an empty folder: listing it again later fetches the children.
func TestChildrenNotCachedWhenFetchFails(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_offline_children"
	dir, cache := mountMockManual(t, remoteDir)
	require.Empty(t, dirNames(t, dir))

	mock.Mkdir(remoteDir + "/Litterature")
	for _, n := range noteNames(10) {
		mock.PutFile(remoteDir+"/Litterature/"+n, []byte(n))
	}
	// the new folder arrives while the filesystem is offline
	cache.Lock()
	cache.offline = true
	cache.Unlock()
	cache.deltaLink += "&order=reverse"
	runOnePoll(t, cache)

	folderID := mock.ID(remoteDir + "/Litterature")
	folder := cache.GetID(folderID)
	require.NotNil(t, folder)

	// listing without working credentials gives an empty result ...
	children, err := cache.GetChildrenID(folderID, &graph.Auth{})
	require.NoError(t, err)
	assert.Empty(t, children)
	assert.False(t, folder.childrenKnown(), "failed fetch was cached as the folder's children")
	// ... also when the folder gets items added in the meantime
	cache.InsertChild(folderID, NewInode("local_only", 0644, folder))
	assert.False(t, folder.childrenKnown(), "single insert made the list look complete")

	// back online: the listing has everything
	cache.Lock()
	cache.offline = false
	cache.Unlock()
	assert.ElementsMatch(t, noteNames(10), dirNames(t, filepath.Join(dir, "Litterature")))
}

// pollWithoutApplying advances the delta link to the present, dropping what
// was reported (a missed update).
func pollWithoutApplying(t *testing.T, cache *Filesystem) {
	t.Helper()
	for {
		_, cont, err := cache.pollDeltas(auth)
		require.NoError(t, err)
		if !cont {
			return
		}
	}
}

// Resync over D-Bus brings back items that were missed, removes items that were
// deleted on the server unnoticed, and keeps locally modified files.
func TestResyncDBus(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_resync_dbus"
	addr := startPrivateBus(t)
	mock.Mkdir(remoteDir + "/sub")
	mock.PutFile(remoteDir+"/sub/gone.txt", []byte("g"))
	mock.PutFile(remoteDir+"/sub/stays.txt", []byte("s"))
	mock.PutFile(remoteDir+"/modified.txt", []byte("m"))
	dir, cache := mountMockManual(t, remoteDir)
	mnt := dir[:len(dir)-len(remoteDir)]
	require.ElementsMatch(t, []string{"sub", "modified.txt"}, dirNames(t, dir))
	require.ElementsMatch(t, []string{"gone.txt", "stays.txt"}, dirNames(t, filepath.Join(dir, "sub")))

	// a file with unsaved local changes
	mock.PauseUploads(remoteDir)
	defer mock.ResumeUploads(remoteDir)
	fd, err := os.OpenFile(filepath.Join(dir, "modified.txt"), os.O_WRONLY|os.O_TRUNC, 0644)
	require.NoError(t, err)
	_, err = fd.WriteString("edited")
	require.NoError(t, err)

	// server side changes that we never hear about
	mock.PutFile(remoteDir+"/sub/missed.txt", []byte("x"))
	mock.PutFile(remoteDir+"/missed2.txt", []byte("y"))
	mock.Delete(mock.ID(remoteDir + "/sub/gone.txt"))
	mock.Delete(mock.ID(remoteDir + "/modified.txt"))
	pollWithoutApplying(t, cache)

	svc, err := onedriverdbus.Start(busConn(t, addr), mnt, cache)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	obj := busConn(t, addr).Object(svc.Name(), onedriverdbus.ObjectPath)
	require.NoError(t, obj.Call(onedriverdbus.Interface+".Resync", 0).Err)

	go cache.DeltaLoop(time.Hour) // only Resync triggers the poll
	require.Eventually(t, func() bool {
		return len(dirNames(t, filepath.Join(dir, "sub"))) == 2 &&
			contains(dirNames(t, filepath.Join(dir, "sub")), "missed.txt")
	}, 20*time.Second, 200*time.Millisecond, "resync did not bring back the missed file")
	assert.ElementsMatch(t, []string{"missed.txt", "stays.txt"}, dirNames(t, filepath.Join(dir, "sub")),
		"stale file was not removed")
	assert.Contains(t, dirNames(t, dir), "missed2.txt")
	assert.Contains(t, dirNames(t, dir), "modified.txt", "locally modified file was removed")
	content, err := os.ReadFile(filepath.Join(dir, "modified.txt"))
	require.NoError(t, err)
	assert.Equal(t, "edited", string(content))
	assert.False(t, cache.resyncRequested())
	fd.Close()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
