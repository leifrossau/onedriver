package fs

// End-to-end tests that need control over the "server side" of the sync
// (pausing uploads, editing files as if from another device). They therefore
// only run against the mock Graph server, and mount their own filesystem (with
// extended attributes enabled and a fast delta loop) next to the shared one.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountMock mounts a fresh filesystem on top of the mock server. remoteDir is
// created on the server first, and its local path inside the mount returned.
func mountMock(t *testing.T, remoteDir string) string {
	t.Helper()
	if mock == nil {
		t.Skip("needs the mock Graph server (unset ONEDRIVER_TEST_LIVE)")
	}
	mock.Mkdir(remoteDir)

	mnt, err := os.MkdirTemp("", "onedriver-e2e-mount-")
	require.NoError(t, err)
	cache := NewFilesystem(auth, filepath.Join(testDBLoc, "e2e_"+strings.ReplaceAll(t.Name(), "/", "_")))
	server, err := fuse.NewServer(cache, mnt, &fuse.MountOptions{
		Name:          "onedriver",
		FsName:        "onedriver",
		MaxBackground: 1024,
	})
	require.NoError(t, err)
	go server.Serve()
	require.NoError(t, server.WaitMount())
	go cache.DeltaLoop(500 * time.Millisecond)

	t.Cleanup(func() {
		if server.Unmount() != nil {
			exec.Command("fusermount3", "-zu", mnt).Run()
		}
		os.Remove(mnt)
	})
	return filepath.Join(mnt, remoteDir)
}

func getState(t *testing.T, fname string) string {
	t.Helper()
	buf := make([]byte, 64)
	n, err := syscall.Getxattr(fname, "user.onedriver.state", buf)
	require.NoError(t, err)
	return string(buf[:n])
}

// A file's sync state should go cloud -> local (downloaded) -> modified (open
// with unsaved edits) -> uploading (closed, upload held up at the server) ->
// local (upload finished).
func TestXAttrStateTransitions(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_xattr"
	dir := mountMock(t, remoteDir)
	id := mock.PutFile(remoteDir+"/file.txt", []byte("hello from the cloud"))
	fname := filepath.Join(dir, "file.txt")

	// not downloaded yet
	require.Eventually(t, func() bool {
		_, err := os.Stat(fname)
		return err == nil
	}, 10*time.Second, 100*time.Millisecond)
	assert.Equal(t, "cloud", getState(t, fname))

	// the id attribute exposes the OneDrive item ID
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(fname, "user.onedriver.id", buf)
	require.NoError(t, err)
	assert.Equal(t, id, string(buf[:n]))

	// reading downloads it
	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "hello from the cloud", string(content))
	assert.Equal(t, "local", getState(t, fname))

	// hold uploads at the server so we can observe the intermediate states
	mock.PauseUploads(remoteDir)
	defer mock.ResumeUploads(remoteDir)

	file, err := os.OpenFile(fname, os.O_WRONLY|os.O_TRUNC, 0644)
	require.NoError(t, err)
	_, err = file.WriteString("edited locally")
	require.NoError(t, err)
	assert.Equal(t, "modified", getState(t, fname), "open file with unsaved writes")

	require.NoError(t, file.Close()) // flush queues the upload
	assert.Equal(t, "uploading", getState(t, fname), "upload queued/blocked at the server")

	mock.ResumeUploads(remoteDir)
	assert.Eventually(t, func() bool {
		return getState(t, fname) == "local"
	}, 30*time.Second, 200*time.Millisecond, "state did not return to local after upload")
	remote, ok := mock.Content(id)
	require.True(t, ok)
	assert.Equal(t, "edited locally", string(remote))
}

// If a file is changed on the server while local changes are still waiting to
// be uploaded, the local version must win and the server version must be kept
// as a "(conflict ...)" copy.
func TestDeltaConflictCopy(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_conflict"
	dir := mountMock(t, remoteDir)
	id := mock.PutFile(remoteDir+"/doc.txt", []byte("original"))
	fname := filepath.Join(dir, "doc.txt")

	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	require.Equal(t, "original", string(content))

	mock.PauseUploads(remoteDir)
	defer mock.ResumeUploads(remoteDir)

	// local edit, upload queued but stuck
	require.NoError(t, os.WriteFile(fname, []byte("local edit"), 0644))
	assert.Equal(t, "uploading", getState(t, fname))

	// another device edits the same file in the meantime
	mock.Modify(id, []byte("remote edit"))

	// the next delta poll must produce a conflict copy holding the server
	// version, while the original keeps the local content
	var conflictName string
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "doc (conflict ") && strings.HasSuffix(e.Name(), ").txt") {
				conflictName = e.Name()
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "no conflict copy appeared")

	copyContent, err := os.ReadFile(filepath.Join(dir, conflictName))
	require.NoError(t, err)
	assert.Equal(t, "remote edit", string(copyContent), "conflict copy should hold the server version")
	local, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "local edit", string(local), "local content must not be overwritten")

	// once uploads continue, the local version replaces the server's, and the
	// conflict copy is uploaded as a new file
	mock.ResumeUploads(remoteDir)
	assert.Eventually(t, func() bool {
		remote, _ := mock.Content(id)
		saved, ok := mock.ContentPath(remoteDir + "/" + conflictName)
		return string(remote) == "local edit" && ok && string(saved) == "remote edit"
	}, 30*time.Second, 200*time.Millisecond,
		fmt.Sprintf("uploads did not settle, server has: %v", mock.List(remoteDir)))

	// and the local file is still the local edit after everything settled
	local, err = os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "local edit", string(local))
}

// A server-side edit with no local changes is accepted without any conflict
// copy being created.
func TestDeltaRemoteEditNoConflict(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_noconflict"
	dir := mountMock(t, remoteDir)
	id := mock.PutFile(remoteDir+"/doc.txt", []byte("original"))
	fname := filepath.Join(dir, "doc.txt")

	content, err := os.ReadFile(fname)
	require.NoError(t, err)
	require.Equal(t, "original", string(content))

	mock.Modify(id, []byte("changed elsewhere"))
	require.Eventually(t, func() bool {
		content, err := os.ReadFile(fname)
		return err == nil && string(content) == "changed elsewhere"
	}, 30*time.Second, 200*time.Millisecond)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "unexpected conflict copy")
}
