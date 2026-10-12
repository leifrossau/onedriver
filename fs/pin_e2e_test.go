package fs

// End-to-end tests of pinning ("always keep on this device") and eviction
// ("free up space"), against the mock Graph server.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pinnedAttr = "user.onedriver.pinned"
	evictAttr  = "user.onedriver.evict"
)

func getPinned(t *testing.T, path string) string {
	t.Helper()
	buf := make([]byte, 16)
	n, err := syscall.Getxattr(path, pinnedAttr, buf)
	require.NoError(t, err)
	return string(buf[:n])
}

func setPinned(t *testing.T, path string, pinned bool) {
	t.Helper()
	value := []byte("0")
	if pinned {
		value = []byte("1")
	}
	require.NoError(t, syscall.Setxattr(path, pinnedAttr, value, 0))
}

func evict(path string) error {
	return syscall.Setxattr(path, evictAttr, []byte("1"), 0)
}

// waitExists waits until a path shows up in the mount (via delta or listing).
func waitExists(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "%s never appeared", path)
}

// cached returns the content cached for an ID, without going through FUSE
// (which would download it).
func cached(f *Filesystem, id string) (string, bool) {
	if _, err := f.content.Stat(id); err != nil {
		return "", false
	}
	return string(f.content.Get(id)), true
}

// eventuallyCached waits until the content for id is cached with the given
// content, without the file ever being opened.
func eventuallyCached(t *testing.T, f *Filesystem, id, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, ok := cached(f, id)
		return ok && got == want
	}, 20*time.Second, 100*time.Millisecond, "content of %s was not hydrated", id)
}

// Pinning a file downloads its content in the background, without the file
// being opened.
func TestPinFileHydrates(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_file"
	dir, f := mountMockFS(t, remoteDir)
	id := mock.PutFile(remoteDir+"/file.txt", []byte("keep me around"))
	fname := filepath.Join(dir, "file.txt")
	waitExists(t, fname)

	assert.Equal(t, "cloud", getState(t, fname))
	assert.Equal(t, "0", getPinned(t, fname))

	setPinned(t, fname, true)
	assert.Equal(t, "1", getPinned(t, fname))
	eventuallyCached(t, f, id, "keep me around")
	assert.Equal(t, "local", getState(t, fname))
	assert.Equal(t, "0", getPinned(t, dir), "pinning a file does not pin its folder")

	require.Eventually(t, func() bool {
		st := f.PinStatus()
		return st.PinnedFiles == 1 && st.PendingFiles == 0
	}, 10*time.Second, 100*time.Millisecond, "status: %+v", f.PinStatus())

	// the attribute is listed
	buf := make([]byte, 256)
	n, err := syscall.Listxattr(fname, buf)
	require.NoError(t, err)
	assert.Contains(t, strings.Split(string(buf[:n]), "\x00"), pinnedAttr)
	assert.NotContains(t, strings.Split(string(buf[:n]), "\x00"), evictAttr)

	// the other attributes stay read-only, garbage values are rejected
	assert.Error(t, syscall.Setxattr(fname, "user.onedriver.state", []byte("local"), 0))
	assert.Error(t, syscall.Setxattr(fname, pinnedAttr, []byte("maybe"), 0))

	// removing the attribute unpins
	require.NoError(t, syscall.Removexattr(fname, pinnedAttr))
	assert.Equal(t, "0", getPinned(t, fname))
}

// Pinning a folder pins and downloads its whole subtree (fetching folders
// that were never listed from the server), and keeps doing so for items
// that show up in it later.
func TestPinFolderRecursive(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_folder"
	mock.PutFile(remoteDir+"/pinned/top.txt", []byte("top"))
	mock.PutFile(remoteDir+"/pinned/sub/mid.txt", []byte("mid"))
	deepID := mock.PutFile(remoteDir+"/pinned/sub/deep/deep.txt", []byte("deep"))
	outsideID := mock.PutFile(remoteDir+"/outside.txt", []byte("not pinned"))
	dir, f := mountMockFS(t, remoteDir)
	pinned := filepath.Join(dir, "pinned")
	waitExists(t, pinned)

	setPinned(t, pinned, true)
	// nothing below "pinned" was ever looked up, the worker has to list it
	eventuallyCached(t, f, deepID, "deep")
	eventuallyCached(t, f, mock.ID(remoteDir+"/pinned/top.txt"), "top")
	eventuallyCached(t, f, mock.ID(remoteDir+"/pinned/sub/mid.txt"), "mid")
	assert.Equal(t, "1", getPinned(t, filepath.Join(pinned, "sub", "deep", "deep.txt")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(pinned, "sub")))
	_, isCached := cached(f, outsideID)
	assert.False(t, isCached, "items outside the pinned folder must not be downloaded")
	assert.Equal(t, "0", getPinned(t, filepath.Join(dir, "outside.txt")))

	// a file added on the server later is downloaded as well
	newID := mock.PutFile(remoteDir+"/pinned/sub/later.txt", []byte("added later"))
	eventuallyCached(t, f, newID, "added later")
	assert.Equal(t, "1", getPinned(t, filepath.Join(pinned, "sub", "later.txt")))

	// and so is a new folder with content
	newDeepID := mock.PutFile(remoteDir+"/pinned/newdir/inside.txt", []byte("inside new dir"))
	eventuallyCached(t, f, newDeepID, "inside new dir")

	// locally created items inherit the pin
	local := filepath.Join(pinned, "local.txt")
	require.NoError(t, os.WriteFile(local, []byte("local"), 0644))
	assert.Equal(t, "1", getPinned(t, local))

	// moving a file out of the pinned folder unpins it, moving it back in
	// pins it again
	moved := filepath.Join(dir, "moved.txt")
	require.NoError(t, os.Rename(filepath.Join(pinned, "top.txt"), moved))
	assert.Equal(t, "0", getPinned(t, moved))
	require.NoError(t, os.Rename(moved, filepath.Join(pinned, "top.txt")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(pinned, "top.txt")))

	// unpinning a file inside a pinned folder overrides the folder's pin
	mid := filepath.Join(pinned, "sub", "mid.txt")
	setPinned(t, mid, false)
	assert.Equal(t, "0", getPinned(t, mid))
	assert.Equal(t, "1", getPinned(t, filepath.Join(pinned, "sub")))

	// unpinning the folder unpins everything below it, pinning it again
	// resets the override
	setPinned(t, pinned, false)
	assert.Equal(t, "0", getPinned(t, filepath.Join(pinned, "sub", "later.txt")))
	setPinned(t, pinned, true)
	assert.Equal(t, "1", getPinned(t, mid))
}

// A pinned file that changes on the server (with no local changes) is
// downloaded again without being opened.
func TestPinnedFileRedownloadedOnServerChange(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_update"
	dir, f := mountMockFS(t, remoteDir)
	id := mock.PutFile(remoteDir+"/doc.txt", []byte("version 1"))
	fname := filepath.Join(dir, "doc.txt")
	waitExists(t, fname)

	setPinned(t, fname, true)
	eventuallyCached(t, f, id, "version 1")

	mock.Modify(id, []byte("version 2, a bit longer"))
	eventuallyCached(t, f, id, "version 2, a bit longer")
	assert.Equal(t, "local", getState(t, fname))

	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "version 2, a bit longer", string(content))
}

// Unpinning does not evict, the evict attribute does. Pinned files are not
// evicted.
func TestUnpinAndEvict(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_evict"
	dir, f := mountMockFS(t, remoteDir)
	id := mock.PutFile(remoteDir+"/file.txt", []byte("evict me"))
	fname := filepath.Join(dir, "file.txt")
	waitExists(t, fname)

	setPinned(t, fname, true)
	eventuallyCached(t, f, id, "evict me")

	// pinned content is kept
	assert.ErrorIs(t, evict(fname), syscall.EBUSY)
	assert.Equal(t, "local", getState(t, fname))

	// unpinning alone keeps the content
	setPinned(t, fname, false)
	assert.Equal(t, "local", getState(t, fname))
	_, ok := cached(f, id)
	assert.True(t, ok)

	require.NoError(t, evict(fname))
	assert.Equal(t, "cloud", getState(t, fname))
	_, ok = cached(f, id)
	assert.False(t, ok, "content file must be gone")

	// the file is still there and is downloaded again when read
	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "evict me", string(content))
	assert.Equal(t, "local", getState(t, fname))

	// evicting a folder evicts everything unpinned below it
	require.NoError(t, evict(dir))
	assert.Equal(t, "cloud", getState(t, fname))
}

// Eviction never touches open files or files with local changes / pending
// uploads.
func TestEvictSkipsOpenAndModified(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_evict_skip"
	dir, f := mountMockFS(t, remoteDir)
	openID := mock.PutFile(remoteDir+"/open.txt", []byte("open"))
	mock.PutFile(remoteDir+"/modified.txt", []byte("original"))
	cleanID := mock.PutFile(remoteDir+"/clean.txt", []byte("clean"))
	openName := filepath.Join(dir, "open.txt")
	modifiedName := filepath.Join(dir, "modified.txt")
	cleanName := filepath.Join(dir, "clean.txt")
	for _, name := range []string{openName, modifiedName, cleanName} {
		waitExists(t, name)
		_, err := os.ReadFile(name)
		require.NoError(t, err)
		require.Equal(t, "local", getState(t, name))
	}

	// a file that is open (here: opened, read and closed via a second fd, so
	// the cache fd is closed but the kernel still has the file open)
	fd, err := os.Open(openName)
	require.NoError(t, err)
	defer fd.Close()
	_, err = os.ReadFile(openName)
	require.NoError(t, err)

	// a file with an upload stuck at the server
	mock.PauseUploads(remoteDir + "/modified.txt")
	defer mock.ResumeUploads(remoteDir + "/modified.txt")
	require.NoError(t, os.WriteFile(modifiedName, []byte("changed locally"), 0644))
	require.Equal(t, "uploading", getState(t, modifiedName))

	assert.ErrorIs(t, evict(openName), syscall.EBUSY)
	assert.ErrorIs(t, evict(modifiedName), syscall.EBUSY)
	require.NoError(t, evict(dir))
	assert.Equal(t, "local", getState(t, openName))
	assert.Equal(t, "uploading", getState(t, modifiedName))
	assert.Equal(t, "cloud", getState(t, cleanName))
	got, ok := cached(f, openID)
	assert.True(t, ok)
	assert.Equal(t, "open", got)
	_, ok = cached(f, cleanID)
	assert.False(t, ok)

	// the open file is still fully readable
	buf := make([]byte, 16)
	n, err := fd.ReadAt(buf, 0)
	if err != nil {
		require.Equal(t, io.EOF, err)
	}
	assert.Equal(t, "open", string(buf[:n]))

	// once closed, it can be evicted
	require.NoError(t, fd.Close())
	require.Eventually(t, func() bool {
		return evict(openName) == nil
	}, 5*time.Second, 100*time.Millisecond)
	assert.Equal(t, "cloud", getState(t, openName))

	// the modified file keeps its local content and is uploaded
	mock.ResumeUploads(remoteDir + "/modified.txt")
	require.Eventually(t, func() bool {
		got, _ := mock.ContentPath(remoteDir + "/modified.txt")
		return string(got) == "changed locally"
	}, 30*time.Second, 200*time.Millisecond)
}

// Automatic eviction (cacheLimit) evicts unpinned content only.
func TestAutomaticEviction(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_evict_auto"
	dir, f := mountMockFS(t, remoteDir)
	pinnedID := mock.PutFile(remoteDir+"/pinned.txt", []byte("pinned content"))
	unpinnedID := mock.PutFile(remoteDir+"/unpinned.txt", []byte("unpinned content"))
	pinnedName := filepath.Join(dir, "pinned.txt")
	unpinnedName := filepath.Join(dir, "unpinned.txt")
	waitExists(t, pinnedName)
	waitExists(t, unpinnedName)

	setPinned(t, pinnedName, true)
	eventuallyCached(t, f, pinnedID, "pinned content")
	_, err := os.ReadFile(unpinnedName)
	require.NoError(t, err)
	_, ok := cached(f, unpinnedID)
	require.True(t, ok)

	f.SetCacheOptions(CacheOptions{CacheLimit: 1, EvictInterval: 100 * time.Millisecond})
	require.Eventually(t, func() bool {
		_, ok := cached(f, unpinnedID)
		return !ok
	}, 10*time.Second, 100*time.Millisecond, "unpinned content was not evicted")
	assert.Equal(t, "cloud", getState(t, unpinnedName))
	got, ok := cached(f, pinnedID)
	assert.True(t, ok, "pinned content must never be evicted")
	assert.Equal(t, "pinned content", got)
}

// Pins are stored with the metadata and survive a remount; pinned folders
// are brought up to date with changes made while unmounted.
func TestPinSurvivesRemount(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_pin_remount"
	mock.Mkdir(remoteDir)
	mock.PutFile(remoteDir+"/folder/a.txt", []byte("a"))
	fileID := mock.PutFile(remoteDir+"/single.txt", []byte("single"))
	cacheDir := filepath.Join(testDBLoc, "e2e_"+t.Name())

	mnt, f, unmount := mountMockAt(t, cacheDir)
	t.Cleanup(unmount)
	dir := filepath.Join(mnt, remoteDir)
	waitExists(t, filepath.Join(dir, "single.txt"))
	setPinned(t, filepath.Join(dir, "folder"), true)
	setPinned(t, filepath.Join(dir, "single.txt"), true)
	eventuallyCached(t, f, fileID, "single")
	eventuallyCached(t, f, mock.ID(remoteDir+"/folder/a.txt"), "a")
	unmount()

	// changed on the server while not mounted
	laterID := mock.PutFile(remoteDir+"/folder/later.txt", []byte("while unmounted"))
	mock.Modify(fileID, []byte("single, version 2"))

	mnt2, f2, unmount2 := mountMockAt(t, cacheDir)
	t.Cleanup(unmount2)
	dir2 := filepath.Join(mnt2, remoteDir)

	// the worker picks the stored pins up by itself, before anything is
	// looked up through the mount
	eventuallyCached(t, f2, laterID, "while unmounted")
	eventuallyCached(t, f2, fileID, "single, version 2")

	assert.Equal(t, "1", getPinned(t, filepath.Join(dir2, "folder")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(dir2, "folder", "a.txt")))
	assert.Equal(t, "1", getPinned(t, filepath.Join(dir2, "single.txt")))
	assert.Equal(t, "0", getPinned(t, dir2))
}
