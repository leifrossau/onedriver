package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The verification of cached content survives a restart, so the first Open
// after a restart does not re-hash the whole file.
func TestVerifiedContentPersistsAcrossRestart(t *testing.T) {
	cacheDir := filepath.Join(testDBLoc, "e2e_verified_persist")
	os.RemoveAll(cacheDir)
	content := []byte(strings.Repeat("persist me ", 1000))
	id := mock.PutFile("/verified_persist/a.bin", content)
	rel := filepath.Join("verified_persist", "a.bin")

	mnt, fsys, unmount := mountMockAt(t, cacheDir)
	defer unmount()
	got, err := os.ReadFile(filepath.Join(mnt, rel))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.NotZero(t, fsys.hashCount.Load(), "first read must hash")
	unmount()
	downloads := contentRequests(id)

	// restart: nothing is re-hashed, content is still served from the cache
	mnt, fsys, unmount2 := mountMockAt(t, cacheDir)
	defer unmount2()
	got, err = os.ReadFile(filepath.Join(mnt, rel))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Zero(t, fsys.hashCount.Load(), "verified content must not be re-hashed")
	assert.Equal(t, downloads, contentRequests(id), "must not be downloaded again")
	unmount2()

	// the cache file changes behind our back (same size, new mtime): re-hash
	cached := filepath.Join(cacheDir, "content", id)
	require.NoError(t, os.Chtimes(cached, time.Now(), time.Now().Add(-time.Hour)))
	mnt, fsys, unmount3 := mountMockAt(t, cacheDir)
	defer unmount3()
	got, err = os.ReadFile(filepath.Join(mnt, rel))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.NotZero(t, fsys.hashCount.Load(), "changed mtime must force a re-hash")
	unmount3()

	// corrupted content (new size and mtime) is detected and replaced
	require.NoError(t, os.WriteFile(cached, []byte("garbage"), 0600))
	mnt, _, unmount4 := mountMockAt(t, cacheDir)
	defer unmount4()
	got, err = os.ReadFile(filepath.Join(mnt, rel))
	require.NoError(t, err)
	assert.Equal(t, content, got)
}
