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
	dir, _ := mountMockFS(t, remoteDir)
	return dir
}

// mountMockFS is mountMock, but also returns the mounted Filesystem.
func mountMockFS(t *testing.T, remoteDir string) (string, *Filesystem) {
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
	return filepath.Join(mnt, remoteDir), cache
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

// Regression: a server-side rename that arrives (via delta) after our own
// upload reached the server but before the upload manager noticed it had
// completed used to be lost. applyDelta could not match the delta to the
// still-local-ID inode (the name differs), so it inserted a second inode for
// the new ID; then the upload manager's MoveID(local -> remote) replaced it,
// and the file kept its old name locally even though the delta is never
// repeated.
//
// The race window is up to one upload manager tick, so this loops a few times
// to hit it reliably.
func TestDeltaRenameWhileUploadCompletes(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_rename_race"
	dir := mountMock(t, remoteDir)

	for i := 0; i < 6; i++ {
		oldName, newName := fmt.Sprintf("old%d.txt", i), fmt.Sprintf("new%d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, oldName), []byte("content"), 0644))

		var id string
		require.Eventually(t, func() bool {
			id = mock.ID(remoteDir + "/" + oldName)
			return id != ""
		}, 10*time.Second, 5*time.Millisecond)
		require.NoError(t, mock.Move(id, newName, ""))

		assert.Eventuallyf(t, func() bool {
			_, errNew := os.Stat(filepath.Join(dir, newName))
			_, errOld := os.Stat(filepath.Join(dir, oldName))
			return errNew == nil && errOld != nil
		}, 15*time.Second, 200*time.Millisecond,
			"iteration %d: server-side rename of %s to %s was lost locally", i, oldName, newName)
	}
}

// BUG: UploadManager.inFlight leaks a slot every time an errored upload is
// retried (upload_manager.go: inFlight++ whenever a NotStarted session is
// started, including after uploadErrored resets it, but only finishUpload
// decrements). The same happens whenever a started session is replaced by a
// newer one for the same item. After maxUploadsInFlight leaks the manager
// silently stops starting any upload, so one file whose upload keeps failing
// stalls the whole mount.
//
// Skipped by default; run with ONEDRIVER_TEST_KNOWN_BUGS=1 to see it fail.
func TestUploadManagerInFlightLeak(t *testing.T) {
	t.Parallel()
	if os.Getenv("ONEDRIVER_TEST_KNOWN_BUGS") != "1" {
		t.Skip("known bug (see comment), run with ONEDRIVER_TEST_KNOWN_BUGS=1")
	}
	const remoteDir = "/e2e_inflight"
	dir := mountMock(t, remoteDir)

	// one file whose upload is rejected until the manager has retried it and
	// finally given up on it
	mock.FailUploads(remoteDir+"/bad.txt", 400)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.txt"), []byte("bad"), 0644))
	require.Eventually(t, func() bool {
		attempts := 0
		for _, r := range mock.Requests() {
			if strings.HasPrefix(r, "PUT ") && strings.Contains(r, "bad.txt") {
				attempts++
			}
		}
		return attempts >= maxUploadsInFlight
	}, 60*time.Second, 200*time.Millisecond, "upload manager did not retry the failing upload")
	mock.StopFailingUploads(remoteDir + "/bad.txt")

	// nothing else is uploading, so a brand new file must upload
	require.NoError(t, os.WriteFile(filepath.Join(dir, "after.txt"), []byte("after"), 0644))
	assert.Eventually(t, func() bool {
		got, ok := mock.ContentPath(remoteDir + "/after.txt")
		return ok && string(got) == "after"
	}, 20*time.Second, 200*time.Millisecond, "upload manager is stuck: inFlight slots leaked")
}

// Regression: a file that is rewritten while its first upload is in flight
// was never uploaded again if the server-side copy showed up in delta before
// the upload manager noticed the first upload completed: applyDelta calls
// MoveID(local -> remote) ("Local item already exists under different ID"),
// and MoveID used DeleteID, which calls UploadManager.CancelUpload and so
// dropped the queued second session. The local copy had the new content (and
// hasChanges == false), the server kept the old one, silently. MoveID now
// re-keys pending uploads to the new ID instead.
func TestRewriteDuringUploadIsNotLost(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_rewrite"
	dir := mountMock(t, remoteDir)
	fname := filepath.Join(dir, "file.txt")
	remote := remoteDir + "/file.txt"

	mock.PauseUploads(remote)
	defer mock.ResumeUploads(remote)
	require.NoError(t, os.WriteFile(fname, []byte("first"), 0644))
	require.Eventually(t, func() bool { // first upload started, held at the server
		for _, r := range mock.Requests() {
			if strings.HasPrefix(r, "PUT ") && strings.Contains(r, "file.txt") {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, os.WriteFile(fname, []byte("second"), 0644))
	mock.ResumeUploads(remote)

	assert.Eventually(t, func() bool {
		got, _ := mock.ContentPath(remote)
		return string(got) == "second"
	}, 20*time.Second, 200*time.Millisecond, "the second write was never uploaded")
	// the second upload must target the existing item, not create a copy
	assert.Equal(t, []string{"file.txt"}, mock.List(remoteDir))
	local, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.Equal(t, "second", string(local))
}
