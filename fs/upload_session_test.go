package fs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUploadSession verifies that the basic functionality of uploads works correctly.
func TestUploadSession(t *testing.T) {
	t.Parallel()
	testDir, err := fs.GetPath("/onedriver_tests", auth)
	require.NoError(t, err)

	inode := NewInode("uploadSessionSmall.txt", 0644, testDir)
	data := []byte("our super special data")
	inode.setContent(fs, data)
	mtime := inode.ModTime()

	session, err := NewUploadSession(inode, &data)
	require.NoError(t, err)
	err = session.Upload(auth)
	require.NoError(t, err)
	if isLocalID(session.ID) {
		t.Fatalf("The session's ID was somehow still local following an upload: %s\n",
			session.ID)
	}
	if sessionMtime := uint64(session.ModTime.Unix()); sessionMtime != mtime {
		t.Errorf("session modtime changed - before: %d - after: %d", mtime, sessionMtime)
	}

	resp, _, err := graph.GetItemContent(session.ID, auth)
	require.NoError(t, err)
	if !bytes.Equal(data, resp) {
		t.Fatalf("Data mismatch. Original content: %s\nRemote content: %s\n", data, resp)
	}

	// item now has a new id following the upload. We just change the ID here
	// because thats part of the UploadManager functionality and gets tested elsewhere.
	inode.DriveItem.ID = session.ID

	// we overwrite and upload again to test uploading with the new remote id
	newData := []byte("new data is extra long so it covers the old one completely")
	inode.setContent(fs, newData)

	session2, err := NewUploadSession(inode, &newData)
	require.NoError(t, err)
	err = session2.Upload(auth)
	require.NoError(t, err)

	resp, _, err = graph.GetItemContent(session.ID, auth)
	require.NoError(t, err)
	if !bytes.Equal(newData, resp) {
		t.Fatalf("Data mismatch. Original content: %s\nRemote content: %s\n", newData, resp)
	}
}

// TestUploadSessionSmallFS verifies is the same test as TestUploadSessionSmall, but uses
// the filesystem itself to perform the uploads instead of testing the internal upload
// functions directly
func TestUploadSessionSmallFS(t *testing.T) {
	t.Parallel()
	data := []byte("super special data for upload test 2")
	err := os.WriteFile(filepath.Join(TestDir, "uploadSessionSmallFS.txt"), data, 0644)
	require.NoError(t, err)

	// wait for the upload (it starts on the next upload manager tick, and may
	// queue behind other uploads)
	waitForRemoteContent := func(want []byte) {
		t.Helper()
		require.Eventuallyf(t, func() bool {
			item, err := graph.GetItemPath("/onedriver_tests/uploadSessionSmallFS.txt", auth)
			if err != nil || item == nil {
				return false
			}
			content, _, err := graph.GetItemContent(item.ID, auth)
			return err == nil && bytes.Equal(content, want)
		}, retrySeconds, time.Second, "Remote content never matched \"%s\"", want)
	}
	waitForRemoteContent(data)
	waitForRemoteID(t, "/onedriver_tests/uploadSessionSmallFS.txt")

	// upload it again to ensure uploads with an existing remote id succeed
	data = []byte("more super special data")
	err = os.WriteFile(filepath.Join(TestDir, "uploadSessionSmallFS.txt"), data, 0644)
	require.NoError(t, err)
	waitForRemoteContent(data)
}

// copy large file inside onedrive mount, then verify that we can still
// access selected lines
func TestUploadSessionLargeFS(t *testing.T) {
	t.Parallel()
	fname := filepath.Join(TestDir, "dmel.fa")
	require.NoError(t, exec.Command("cp", "dmel.fa", fname).Run())

	contents, err := os.ReadFile(fname)
	require.NoError(t, err)

	header := ">X dna:chromosome chromosome:BDGP6.22:X:1:23542271:1 REF"
	if string(contents[:len(header)]) != header {
		t.Fatalf("Could not read FASTA header. Wanted \"%s\", got \"%s\"\n",
			header, string(contents[:len(header)]))
	}

	final := "AAATAAAATAC\n" // makes yucky test output, but is the final line
	match := string(contents[len(contents)-len(final):])
	if match != final {
		t.Fatalf("Could not read final line of FASTA. Wanted \"%s\", got \"%s\"\n",
			final, match)
	}

	st, _ := os.Stat(fname)
	if st.Size() == 0 {
		t.Fatal("File size cannot be 0.")
	}

	// poll endpoint to make sure it has a size greater than 0
	size := uint64(len(contents))
	var item *graph.DriveItem
	assert.Eventually(t, func() bool {
		item, _ = graph.GetItemPath("/onedriver_tests/dmel.fa", auth)
		inode := NewInodeDriveItem(item)
		return item != nil && inode.Size() == size
	}, 120*time.Second, time.Second, "Upload session did not complete successfully!")

	// test multipart downloads as a bonus part of the test
	downloaded, _, err := graph.GetItemContent(item.ID, auth)
	assert.NoError(t, err)
	assert.Equal(t, graph.QuickXORHash(&contents), graph.QuickXORHash(&downloaded),
		"Downloaded content did not match original content.")
}

// TestUploadSessionLargeUsesFinalResponse checks that a large (chunked) upload
// uses the final chunk's response instead of refetching the item, and that
// cancelling a started session sends an unauthenticated DELETE to the
// (absolute) upload URL.
func TestUploadSessionLargeUsesFinalResponse(t *testing.T) {
	t.Parallel()
	if mock == nil {
		t.Skip("requires the mock Graph server")
	}
	testDir, err := fs.GetPath("/onedriver_tests", auth)
	require.NoError(t, err)

	inode := NewInode("uploadSessionLarge.bin", 0644, testDir)
	data := bytes.Repeat([]byte("0123456789abcdef"), int(uploadLargeSize)/16+1024)
	inode.setContent(fs, data)

	before := len(mock.Requests())
	session, err := NewUploadSession(inode, &data)
	require.NoError(t, err)
	require.NoError(t, session.Upload(auth))
	require.Equal(t, uploadComplete, session.getState())
	assert.False(t, isLocalID(session.ID))
	assert.NotEmpty(t, session.ETag)

	// a refetch of the item would show up as a GET after the chunk PUT
	var sawPut bool
	for _, r := range mock.Requests()[before:] {
		if strings.HasPrefix(r, "PUT /upload/") {
			sawPut = true
		} else if sawPut && strings.HasPrefix(r, "GET ") && !strings.Contains(r, "/delta") &&
			strings.Contains(r, session.ID) && !strings.Contains(r, "/children") {
			// (other tests run in parallel, only requests for this item count)
			t.Errorf("item was refetched after the final chunk: %s", r)
		}
	}
	assert.True(t, sawPut, "no chunk upload seen")

	// cancel must reach the server
	session2, err := NewUploadSession(inode, &data)
	require.NoError(t, err)
	session2.UploadURL = mock.HTTP.URL + "/upload/does-not-matter"
	session2.setState(uploadStarted, nil)
	session2.cancel(auth)
	assert.Eventually(t, func() bool {
		for _, r := range mock.Requests() {
			if r == "DELETE /upload/does-not-matter" {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "cancel never hit the upload URL")
}
