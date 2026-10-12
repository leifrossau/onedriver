package fs

// More end-to-end tests of pinning and eviction: overrides, moves, renames,
// deletes, conflicts, failing downloads, offline operation, restarts and the
// xattr interface. Every test works in its own remote directory.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// itemID returns the OneDrive ID of a path in the mount.
func itemID(t *testing.T, path string) string {
	t.Helper()
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(path, "user.onedriver.id", buf)
	require.NoError(t, err)
	return string(buf[:n])
}

// contentRequests counts the content downloads the server saw for an item.
func contentRequests(id string) int {
	count := 0
	for _, r := range mock.Requests() {
		if strings.HasPrefix(r, "GET ") && strings.Contains(r, "/items/"+id+"/content") {
			count++
		}
	}
	return count
}

// requestsFor counts all requests that mention an item ID.
func requestsFor(id string) int {
	count := 0
	for _, r := range mock.Requests() {
		if strings.Contains(r, id) {
			count++
		}
	}
	return count
}

// notCachedFor asserts that content is not cached for the whole duration.
func notCachedFor(t *testing.T, f *Filesystem, id string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_, ok := cached(f, id)
		require.False(t, ok, "content of %s must not be downloaded", id)
		time.Sleep(100 * time.Millisecond)
	}
}

func eventuallyPinned(t *testing.T, path, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		buf := make([]byte, 16)
		n, err := syscall.Getxattr(path, pinnedAttr, buf)
		return err == nil && string(buf[:n]) == want
	}, 15*time.Second, 100*time.Millisecond, "%s: pinned never became %s", path, want)
}

// An explicit unpin inside a pinned folder sticks: the file is neither
// downloaded again nor re-pinned by anything but pinning an ancestor again.
func TestPinExplicitUnpinInsidePinnedFolder(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_override"
	aID := mock.PutFile(remoteDir+"/pinned/a.txt", []byte("aaa"))
	bID := mock.PutFile(remoteDir+"/pinned/b.txt", []byte("bbb"))
	dir, f := mountMockFS(t, remoteDir)
	pinned := filepath.Join(dir, "pinned")
	a, b := filepath.Join(pinned, "a.txt"), filepath.Join(pinned, "b.txt")
	waitExists(t, pinned)

	setPinned(t, pinned, true)
	eventuallyCached(t, f, aID, "aaa")
	eventuallyCached(t, f, bID, "bbb")

	setPinned(t, a, false)
	assert.Equal(t, "0", getPinned(t, a))
	assert.Equal(t, "1", getPinned(t, b))
	assert.Equal(t, "1", getPinned(t, pinned))
	// content stays until freed explicitly, which is now allowed for a
	require.NoError(t, evict(a))
	assert.ErrorIs(t, evict(b), syscall.EBUSY)
	assert.Equal(t, "cloud", getState(t, a))

	// a stays evicted, also when the server changes it
	mock.Modify(aID, []byte("aaa v2"))
	time.Sleep(3 * time.Second) // delta + several worker rounds
	_, ok := cached(f, aID)
	assert.False(t, ok, "unpinned file inside a pinned folder was downloaded")
	assert.Equal(t, "0", getPinned(t, a))

	// pinning the parent again (even though it is pinned already) resets the
	// override
	setPinned(t, pinned, true)
	assert.Equal(t, "1", getPinned(t, a))
	eventuallyCached(t, f, aID, "aaa v2")
}

// Moving files into and out of a pinned folder through the mount.
func TestPinLocalMoveInAndOut(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_move_local"
	inID := mock.PutFile(remoteDir+"/pinned/in.txt", []byte("inside"))
	outID := mock.PutFile(remoteDir+"/other/out.txt", []byte("outside"))
	dir, f := mountMockFS(t, remoteDir)
	pinned := filepath.Join(dir, "pinned")
	other := filepath.Join(dir, "other")
	waitExists(t, pinned)
	waitExists(t, other)

	setPinned(t, pinned, true)
	eventuallyCached(t, f, inID, "inside")

	// out: no longer pinned, content stays cached
	moved := filepath.Join(other, "was_pinned.txt")
	require.NoError(t, os.Rename(filepath.Join(pinned, "in.txt"), moved))
	assert.Equal(t, "0", getPinned(t, moved))
	assert.Equal(t, "local", getState(t, moved))
	require.NoError(t, evict(moved), "no longer pinned, so it can be evicted")
	assert.Equal(t, "cloud", getState(t, moved))
	notCachedFor(t, f, inID, 2500*time.Millisecond)

	// in: pinned and downloaded without being opened
	_, ok := cached(f, outID)
	require.False(t, ok)
	into := filepath.Join(pinned, "now_pinned.txt")
	require.NoError(t, os.Rename(filepath.Join(other, "out.txt"), into))
	assert.Equal(t, "1", getPinned(t, into))
	eventuallyCached(t, f, outID, "outside")
	assert.Equal(t, "local", getState(t, into))
}

// The same for moves made on another device, delivered via delta.
func TestPinServerMoveInAndOut(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_move_server"
	pinnedID := mock.Mkdir(remoteDir + "/pinned")
	otherID := mock.Mkdir(remoteDir + "/other")
	inID := mock.PutFile(remoteDir+"/pinned/in.txt", []byte("inside"))
	outID := mock.PutFile(remoteDir+"/other/out.txt", []byte("outside"))
	dir, f := mountMockFS(t, remoteDir)
	pinned := filepath.Join(dir, "pinned")
	other := filepath.Join(dir, "other")
	waitExists(t, pinned)
	waitExists(t, other)
	_, err := os.ReadDir(other) // make the folder's content known
	require.NoError(t, err)

	setPinned(t, pinned, true)
	eventuallyCached(t, f, inID, "inside")

	// out
	require.NoError(t, mock.Move(inID, "left.txt", otherID))
	waitExists(t, filepath.Join(other, "left.txt"))
	eventuallyPinned(t, filepath.Join(other, "left.txt"), "0")
	_, err = os.Stat(filepath.Join(pinned, "in.txt"))
	assert.Error(t, err)

	// in
	require.NoError(t, mock.Move(outID, "arrived.txt", pinnedID))
	waitExists(t, filepath.Join(pinned, "arrived.txt"))
	eventuallyPinned(t, filepath.Join(pinned, "arrived.txt"), "1")
	eventuallyCached(t, f, outID, "outside")
}

// Renaming a pinned folder keeps it and everything in it pinned.
func TestPinRenamePinnedFolder(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_rename"
	topID := mock.PutFile(remoteDir+"/local_old/sub/file.txt", []byte("one"))
	srvID := mock.PutFile(remoteDir+"/server_old/file.txt", []byte("two"))
	srvFolder := mock.ID(remoteDir + "/server_old")
	dir, f := mountMockFS(t, remoteDir)
	waitExists(t, filepath.Join(dir, "local_old"))
	waitExists(t, filepath.Join(dir, "server_old"))

	setPinned(t, filepath.Join(dir, "local_old"), true)
	setPinned(t, filepath.Join(dir, "server_old"), true)
	eventuallyCached(t, f, topID, "one")
	eventuallyCached(t, f, srvID, "two")

	// local rename
	require.NoError(t, os.Rename(filepath.Join(dir, "local_old"), filepath.Join(dir, "local_new")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(dir, "local_new")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(dir, "local_new", "sub", "file.txt")))
	assert.Equal(t, "local", getState(t, filepath.Join(dir, "local_new", "sub", "file.txt")))
	assert.ErrorIs(t, evict(filepath.Join(dir, "local_new", "sub", "file.txt")), syscall.EBUSY)

	// server-side rename
	require.NoError(t, mock.Move(srvFolder, "server_new", ""))
	waitExists(t, filepath.Join(dir, "server_new"))
	eventuallyPinned(t, filepath.Join(dir, "server_new"), "1")
	eventuallyPinned(t, filepath.Join(dir, "server_new", "file.txt"), "1")
	assert.Equal(t, "local", getState(t, filepath.Join(dir, "server_new", "file.txt")))

	// and still hydrated for new content
	mock.Modify(srvID, []byte("two v2"))
	eventuallyCached(t, f, srvID, "two v2")
	assert.Equal(t, 2, f.PinStatus().PinnedFiles)
}

// Deleting pinned items on the server drops them from the pin counts, and
// the worker does not keep asking the server about them.
func TestPinServerDelete(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_delete"
	fileID := mock.PutFile(remoteDir+"/file.txt", []byte("file"))
	mock.PutFile(remoteDir+"/folder/one.txt", []byte("one"))
	mock.PutFile(remoteDir+"/folder/two.txt", []byte("two"))
	oneID := mock.ID(remoteDir + "/folder/one.txt")
	folderID := mock.ID(remoteDir + "/folder")
	dir, f := mountMockFS(t, remoteDir)
	waitExists(t, filepath.Join(dir, "file.txt"))
	waitExists(t, filepath.Join(dir, "folder"))

	setPinned(t, filepath.Join(dir, "file.txt"), true)
	setPinned(t, filepath.Join(dir, "folder"), true)
	eventuallyCached(t, f, fileID, "file")
	eventuallyCached(t, f, oneID, "one")
	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.PinnedFiles == 3 && st.PendingFiles == 0
	}, 15*time.Second, 100*time.Millisecond, "%+v", f.PinStatus())

	mock.Delete(fileID)
	require.Eventually(t, func() bool { return f.PinStatus().PinnedFiles == 2 },
		15*time.Second, 100*time.Millisecond, "%+v", f.PinStatus())
	mock.Delete(folderID)
	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.PinnedFiles == 0 && st.PendingFiles == 0
	}, 15*time.Second, 100*time.Millisecond, "%+v", f.PinStatus())

	// the worker goes quiet
	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.Queued == 0 && st.Active == 0
	}, 10*time.Second, 100*time.Millisecond, "worker still busy: %+v", f.PinStatus())
	count := func() int { return requestsFor(fileID) + requestsFor(folderID) + requestsFor(oneID) }
	before := count()
	time.Sleep(5 * time.Second)
	assert.Equal(t, before, count(), "requests for deleted items keep coming")
	_, err := os.Stat(filepath.Join(dir, "folder"))
	assert.Error(t, err)
}

// A pinned file with local changes that is also changed on the server: the
// local version wins, the server version becomes a conflict copy, which as a
// new item inside the pinned folder is pinned and has its content locally.
// Nothing is evicted or overwritten in the process.
func TestPinConflictCopyIsHydrated(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_conflict"
	id := mock.PutFile(remoteDir+"/pinned/doc.txt", []byte("original"))
	dir, f := mountMockFS(t, remoteDir)
	pinned := filepath.Join(dir, "pinned")
	fname := filepath.Join(pinned, "doc.txt")
	waitExists(t, fname)
	setPinned(t, pinned, true)
	eventuallyCached(t, f, id, "original")

	mock.PauseUploads(remoteDir)
	defer mock.ResumeUploads(remoteDir)
	require.NoError(t, os.WriteFile(fname, []byte("local edit"), 0644))
	require.Equal(t, "uploading", getState(t, fname))
	mock.Modify(id, []byte("remote edit"))

	var conflict string
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(pinned)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "doc (conflict ") {
				conflict = filepath.Join(pinned, e.Name())
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "no conflict copy appeared")

	assert.Equal(t, "1", getPinned(t, conflict))
	assert.NotEqual(t, "cloud", getState(t, conflict), "conflict copy must be hydrated")
	copyID := itemID(t, conflict)
	got, ok := cached(f, copyID)
	assert.True(t, ok)
	assert.Equal(t, "remote edit", got)

	// local version untouched, still waiting for its upload
	assert.Equal(t, "uploading", getState(t, fname))
	got, ok = cached(f, id)
	assert.True(t, ok)
	assert.Equal(t, "local edit", got)
	assert.ErrorIs(t, evict(fname), syscall.EBUSY)
	assert.ErrorIs(t, evict(conflict), syscall.EBUSY)

	mock.ResumeUploads(remoteDir)
	require.Eventually(t, func() bool {
		remote, _ := mock.Content(id)
		return string(remote) == "local edit"
	}, 30*time.Second, 200*time.Millisecond)
	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "local edit", string(content))
	assert.Equal(t, "1", getPinned(t, fname))
}

// A failing download is retried with backoff (not in a tight loop) until it
// works, and the counts reflect the file as pending in the meantime.
func TestPinDownloadFailureBackoff(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_dlfail"
	id := mock.PutFile(remoteDir+"/file.txt", []byte("eventually"))
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "file.txt")
	waitExists(t, fname)

	mock.FailDownloads(remoteDir, 503)
	defer mock.StopFailingDownloads(remoteDir)
	setPinned(t, fname, true)

	require.Eventually(t, func() bool { return contentRequests(id) >= 1 },
		15*time.Second, 50*time.Millisecond, "worker never tried to download")
	time.Sleep(7 * time.Second)
	attempts := contentRequests(id)
	// first attempt at 0s, then backed off: 2s, 6s, ... plus whatever retries
	// the HTTP layer does inside one attempt
	assert.LessOrEqual(t, attempts, 16, "worker is hammering the server")
	_, ok := cached(f, id)
	assert.False(t, ok)
	st := f.PinStatus()
	assert.Equal(t, 1, st.PinnedFiles)
	assert.Equal(t, 1, st.PendingFiles)

	mock.StopFailingDownloads(remoteDir)
	eventuallyCached(t, f, id, "eventually")
	require.Eventually(t, func() bool { return f.PinStatus().PendingFiles == 0 },
		10*time.Second, 100*time.Millisecond)
	settled := contentRequests(id)
	time.Sleep(3 * time.Second)
	assert.Equal(t, settled, contentRequests(id), "downloads continue after success")
}

// While offline nothing is downloaded; when the connection is back, the
// pinned content arrives.
func TestPinOfflinePausesHydration(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_offline"
	mock.Mkdir(remoteDir)
	id := mock.PutFile(remoteDir+"/file.txt", []byte("offline test"))
	cacheDir := filepath.Join(testDBLoc, "e2e_"+t.Name())
	// no delta loop: the test controls the offline flag
	mnt, f, unmount := mountMockAtOpts(t, cacheDir, false)
	t.Cleanup(unmount)
	fname := filepath.Join(mnt, remoteDir, "file.txt")
	_, err := os.Stat(fname)
	require.NoError(t, err)

	f.Lock()
	f.offline = true
	f.Unlock()
	setPinned(t, fname, true)
	assert.Equal(t, "1", getPinned(t, fname))
	before := contentRequests(id)
	notCachedFor(t, f, id, 3*time.Second)
	assert.Equal(t, before, contentRequests(id), "downloading while offline")
	assert.Equal(t, 1, f.PinStatus().PendingFiles)

	f.Lock()
	f.offline = false
	f.Unlock()
	f.pins.signal()
	eventuallyCached(t, f, id, "offline test")
	assert.Equal(t, 0, f.PinStatus().PendingFiles)
}

// Evicting a folder frees every clean, unpinned file below it, leaves the
// rest alone, and evicted files download again when read.
func TestEvictSubtree(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_evict_tree"
	cleanID := mock.PutFile(remoteDir+"/tree/clean.txt", []byte("clean"))
	deepID := mock.PutFile(remoteDir+"/tree/sub/deep/deep.txt", []byte("deep content"))
	pinnedID := mock.PutFile(remoteDir+"/tree/pinned.txt", []byte("pinned"))
	openID := mock.PutFile(remoteDir+"/tree/open.txt", []byte("open"))
	mock.PutFile(remoteDir+"/tree/modified.txt", []byte("orig"))
	coldID := mock.PutFile(remoteDir+"/tree/cold.txt", []byte("never read"))
	dir, f := mountMockFS(t, remoteDir)
	tree := filepath.Join(dir, "tree")
	waitExists(t, tree)
	for _, name := range []string{"clean.txt", "pinned.txt", "open.txt", "modified.txt", filepath.Join("sub", "deep", "deep.txt"), "cold.txt"} {
		waitExists(t, filepath.Join(tree, name))
	}
	for _, name := range []string{"clean.txt", "pinned.txt", "open.txt", "modified.txt", filepath.Join("sub", "deep", "deep.txt")} {
		_, err := os.ReadFile(filepath.Join(tree, name))
		require.NoError(t, err)
	}
	setPinned(t, filepath.Join(tree, "pinned.txt"), true)
	fd, err := os.Open(filepath.Join(tree, "open.txt"))
	require.NoError(t, err)
	defer fd.Close()
	mock.PauseUploads(remoteDir + "/tree/modified.txt")
	defer mock.ResumeUploads(remoteDir + "/tree/modified.txt")
	require.NoError(t, os.WriteFile(filepath.Join(tree, "modified.txt"), []byte("edited"), 0644))

	require.NoError(t, evict(tree))
	assert.Equal(t, "cloud", getState(t, filepath.Join(tree, "clean.txt")))
	assert.Equal(t, "cloud", getState(t, filepath.Join(tree, "sub", "deep", "deep.txt")))
	assert.Equal(t, "cloud", getState(t, filepath.Join(tree, "cold.txt")))
	assert.Equal(t, "local", getState(t, filepath.Join(tree, "pinned.txt")))
	assert.Equal(t, "local", getState(t, filepath.Join(tree, "open.txt")))
	assert.Equal(t, "uploading", getState(t, filepath.Join(tree, "modified.txt")))
	for id, want := range map[string]string{pinnedID: "pinned", openID: "open"} {
		got, ok := cached(f, id)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
	for _, id := range []string{cleanID, deepID, coldID} {
		_, ok := cached(f, id)
		assert.False(t, ok)
	}

	// transparently downloaded again
	content, err := os.ReadFile(filepath.Join(tree, "sub", "deep", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep content", string(content))
	assert.Equal(t, "local", getState(t, filepath.Join(tree, "sub", "deep", "deep.txt")))
	content, err = os.ReadFile(filepath.Join(tree, "clean.txt"))
	require.NoError(t, err)
	assert.Equal(t, "clean", string(content))
	// and the modified file kept its edit
	local, err := os.ReadFile(filepath.Join(tree, "modified.txt"))
	require.NoError(t, err)
	assert.Equal(t, "edited", string(local))
}

// With a cache limit, the least recently used unpinned files go first, and
// pinned files stay even if they are the oldest and the cache stays over the
// limit.
func TestAutomaticEvictionLRU(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_evict_lru"
	dir, f := mountMockFS(t, remoteDir)
	body := strings.Repeat("x", 100)
	names := []string{"pinned", "old", "middle", "new"}
	ids := make(map[string]string)
	for _, n := range names {
		ids[n] = mock.PutFile(remoteDir+"/"+n+".txt", []byte(body))
	}
	for _, n := range names {
		waitExists(t, filepath.Join(dir, n+".txt"))
	}
	setPinned(t, filepath.Join(dir, "pinned.txt"), true)
	eventuallyCached(t, f, ids["pinned"], body)
	for _, n := range names[1:] {
		_, err := os.ReadFile(filepath.Join(dir, n+".txt"))
		require.NoError(t, err)
	}
	// make the access order deterministic: pinned is the oldest of all
	now := time.Now()
	for i, n := range names {
		at := now.Add(time.Duration(i-10) * time.Hour)
		require.NoError(t, os.Chtimes(f.content.contentPath(ids[n]), at, at))
	}

	// 4 x 100 bytes cached, limit 250: "old" has to go, then it is 300, so
	// "middle" as well; pinned (oldest) is skipped
	f.SetCacheOptions(CacheOptions{CacheLimit: 250, EvictInterval: 100 * time.Millisecond})
	require.Eventually(t, func() bool {
		_, oldOK := cached(f, ids["old"])
		_, midOK := cached(f, ids["middle"])
		return !oldOK && !midOK
	}, 15*time.Second, 100*time.Millisecond, "LRU files were not evicted")
	_, ok := cached(f, ids["new"])
	assert.True(t, ok, "most recently used file must stay")
	_, ok = cached(f, ids["pinned"])
	assert.True(t, ok, "pinned file must stay")

	// a much lower limit cannot evict the pinned file either
	f.SetCacheOptions(CacheOptions{CacheLimit: 1, EvictInterval: 100 * time.Millisecond})
	require.Eventually(t, func() bool {
		_, ok := cached(f, ids["new"])
		return !ok
	}, 15*time.Second, 100*time.Millisecond)
	time.Sleep(time.Second)
	got, ok := cached(f, ids["pinned"])
	assert.True(t, ok)
	assert.Equal(t, body, got)
	assert.Equal(t, "local", getState(t, filepath.Join(dir, "pinned.txt")))
}

// The pinned attribute's accepted values, on files, folders and the root.
func TestPinXAttrEdgeCases(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_xattr"
	mock.PutFile(remoteDir+"/folder/file.txt", []byte("x"))
	dir, _ := mountMockFS(t, remoteDir)
	file := filepath.Join(dir, "folder", "file.txt")
	waitExists(t, file)
	folder := filepath.Join(dir, "folder")

	// garbage is rejected and changes nothing
	for _, bad := range []string{"2", "maybe", "-1", "11"} {
		err := syscall.Setxattr(file, pinnedAttr, []byte(bad), 0)
		assert.ErrorIs(t, err, syscall.EINVAL, "value %q", bad)
	}
	assert.Equal(t, "0", getPinned(t, file))

	// accepted spellings. An empty value counts as "0" (setfattr -n x
	// without a value unpins).
	require.NoError(t, syscall.Setxattr(file, pinnedAttr, []byte("yes"), 0))
	assert.Equal(t, "1", getPinned(t, file))
	require.NoError(t, syscall.Setxattr(file, pinnedAttr, []byte{}, 0))
	assert.Equal(t, "0", getPinned(t, file))
	require.NoError(t, syscall.Setxattr(file, pinnedAttr, []byte("true\n"), 0))
	assert.Equal(t, "1", getPinned(t, file))
	require.NoError(t, syscall.Removexattr(file, pinnedAttr))
	assert.Equal(t, "0", getPinned(t, file))

	// other attribute names
	assert.Error(t, syscall.Setxattr(file, "user.onedriver.nonsense", []byte("1"), 0))
	assert.Error(t, syscall.Removexattr(file, "user.onedriver.state"))
	assert.Error(t, syscall.Removexattr(file, evictAttr))
	buf := make([]byte, 8)
	_, err := syscall.Getxattr(file, evictAttr, buf)
	assert.Error(t, err, "evict is write-only")

	// directories report and accept the attribute too
	assert.Equal(t, "0", getPinned(t, folder))
	setPinned(t, folder, true)
	assert.Equal(t, "1", getPinned(t, folder))
	assert.Equal(t, "1", getPinned(t, file))
	// the size probe reports the length
	n, err := syscall.Getxattr(folder, pinnedAttr, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	setPinned(t, folder, false)
	assert.Equal(t, "0", getPinned(t, file))

	// the mount root can be pinned (everything inherits) and unpinned
	root := filepath.Dir(dir)
	assert.Equal(t, "0", getPinned(t, root))
	setPinned(t, root, true)
	assert.Equal(t, "1", getPinned(t, root))
	assert.Equal(t, "1", getPinned(t, dir))
	assert.Equal(t, "1", getPinned(t, file))
	require.NoError(t, syscall.Removexattr(root, pinnedAttr))
	assert.Equal(t, "0", getPinned(t, root))
	assert.Equal(t, "0", getPinned(t, file))
}

// PinStatus follows a folder through pin, hydration and unpin.
func TestPinStatusLifecycle(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_status"
	mock.PutFile(remoteDir+"/folder/a.txt", []byte("12345"))
	mock.PutFile(remoteDir+"/folder/sub/b.txt", []byte("1234567890"))
	mock.PutFile(remoteDir+"/folder/sub/c.txt", []byte("123"))
	mock.PutFile(remoteDir+"/other.txt", []byte("not pinned"))
	dir, f := mountMockFS(t, remoteDir)
	folder := filepath.Join(dir, "folder")
	waitExists(t, folder)
	waitExists(t, filepath.Join(dir, "other.txt"))
	assert.Equal(t, PinStatus{}, f.PinStatus())

	// hold the downloads back to see the pending state
	mock.FailDownloads(remoteDir, 503)
	defer mock.StopFailingDownloads(remoteDir)
	setPinned(t, folder, true)
	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.PinnedFiles == 3 && st.PendingFiles == 3
	}, 15*time.Second, 100*time.Millisecond, "%+v", f.PinStatus())
	st := f.PinStatus()
	assert.EqualValues(t, 18, st.PinnedBytes)
	assert.EqualValues(t, 18, st.PendingBytes)

	mock.StopFailingDownloads(remoteDir)
	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.PinnedFiles == 3 && st.PendingFiles == 0 && st.Queued == 0 && st.Active == 0
	}, 30*time.Second, 100*time.Millisecond, "%+v", f.PinStatus())
	st = f.PinStatus()
	assert.EqualValues(t, 18, st.PinnedBytes)
	assert.EqualValues(t, 0, st.PendingBytes)

	// unpinning a single file inside leaves the others counted
	setPinned(t, filepath.Join(folder, "sub", "b.txt"), false)
	st = f.PinStatus()
	assert.Equal(t, 2, st.PinnedFiles)
	assert.EqualValues(t, 8, st.PinnedBytes)

	setPinned(t, folder, false)
	assert.Equal(t, PinStatus{}, f.PinStatus())
}

// A pinned folder that was not fully downloaded when the filesystem stopped
// is finished after the next mount.
func TestPinRestartFinishesHydration(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_restart"
	mock.Mkdir(remoteDir)
	ids := map[string]string{}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		ids[name] = mock.PutFile(remoteDir+"/folder/sub/"+name, []byte("content "+name))
	}
	cacheDir := filepath.Join(testDBLoc, "e2e_"+t.Name())

	mnt, f, unmount := mountMockAt(t, cacheDir)
	t.Cleanup(unmount)
	folder := filepath.Join(mnt, remoteDir, "folder")
	waitExists(t, folder)
	mock.FailDownloads(remoteDir, 503)
	defer mock.StopFailingDownloads(remoteDir)
	setPinned(t, folder, true)
	require.Eventually(t, func() bool {
		for _, id := range ids {
			if contentRequests(id) > 0 {
				return true
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond, "worker never started")
	for name, id := range ids {
		_, ok := cached(f, id)
		require.False(t, ok, name)
	}
	unmount()

	mock.StopFailingDownloads(remoteDir)
	mnt2, f2, unmount2 := mountMockAt(t, cacheDir)
	t.Cleanup(unmount2)
	for name, id := range ids {
		eventuallyCached(t, f2, id, "content "+name)
	}
	assert.Equal(t, "1", getPinned(t, filepath.Join(mnt2, remoteDir, "folder", "sub", "file0.txt")))
	require.Eventually(t, func() bool {
		st := f2.PinStatus()
		return st.PinnedFiles == 4 && st.PendingFiles == 0
	}, 10*time.Second, 100*time.Millisecond, "%+v", f2.PinStatus())
}
