package fs

// End-to-end tests of on-demand partial reads (see partial.go): only the
// ranges that are read are downloaded, complete files are verified, writes
// download the rest first.

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

const partialTestSize = 20 << 20

// randomContent returns size pseudo-random bytes.
func randomContent(seed int64, size int) []byte {
	data := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(data)
	return data
}

// readAt reads len bytes at off from a file in the mount.
func readAt(t *testing.T, path string, off int64, n int) ([]byte, error) {
	t.Helper()
	fd, err := os.Open(path)
	require.NoError(t, err)
	defer fd.Close()
	buf := make([]byte, n)
	got, err := fd.ReadAt(buf, off)
	return buf[:got], err
}

// remoteHash returns the QuickXorHash the filesystem knows for an item.
func remoteHash(f *Filesystem, id string) string {
	inode := f.GetID(id)
	if inode == nil {
		return ""
	}
	hash, _ := remoteHashOfInode(inode)
	return hash
}

// cachedRanges returns the cached ranges of a partial file (nil if it is not
// partial).
func cachedRanges(f *Filesystem, id string) rangeSet {
	p := f.partialGet(id)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(rangeSet(nil), p.ranges...)
}

// parseRange parses a "bytes=a-b" header into [a, b+1).
func parseRange(t *testing.T, header string) interval {
	t.Helper()
	var a, b int64
	_, err := fmt.Sscanf(header, "bytes=%d-%d", &a, &b)
	require.NoError(t, err, "unexpected Range header %q", header)
	return interval{a, b + 1}
}

// Reading the first 4 KiB of a large file only downloads a few chunks, and
// the file stays in the cloud state.
func TestPartialReadFetchesOnlyWhatIsRead(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_head"
	content := randomContent(1, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	got, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	assert.Equal(t, content[:4096], got)

	n := mock.DownloadedBytes(id)
	assert.GreaterOrEqual(t, n, int64(partialChunk), "at least one chunk")
	assert.LessOrEqual(t, n, int64(1<<20), "only the start of the file may be downloaded")
	assert.Equal(t, "cloud", getState(t, fname))
	assert.True(t, f.isPartial(id))
	assert.True(t, cachedRanges(f, id).contains(0, 4096))

	// the cache file is sparse: it does not take up the file's size
	st, err := f.content.Stat(id)
	require.NoError(t, err)
	assert.EqualValues(t, partialTestSize, st.Size())
	assert.Less(t, diskUsage(st), uint64(4<<20))

	// a read in the middle fetches only around it
	got, err = readAt(t, fname, 10<<20, 4096)
	require.NoError(t, err)
	assert.Equal(t, content[10<<20:10<<20+4096], got)
	assert.LessOrEqual(t, mock.DownloadedBytes(id), int64(2<<20))
	for _, r := range mock.ContentRanges(id) {
		assert.True(t, strings.HasPrefix(r, "bytes="), "only Range requests: %q", r)
	}
	assert.Equal(t, "cloud", getState(t, fname))
}

// Reading a file sequentially downloads it once, in ranges, after which it
// is verified and fully local.
func TestPartialSequentialReadCompletes(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_seq"
	content := randomContent(2, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	got, err := os.ReadFile(fname)
	require.NoError(t, err)
	require.True(t, bytes.Equal(content, got), "content differs")

	require.Eventually(t, func() bool { return getState(t, fname) == "local" },
		10*time.Second, 50*time.Millisecond)
	assert.False(t, f.isPartial(id))
	assert.True(t, f.verifiedNow(id, remoteHash(f, id)), "must be verified")
	assert.EqualValues(t, partialTestSize, mock.DownloadedBytes(id),
		"every byte must be downloaded exactly once")
	ranges := mock.ContentRanges(id)
	assert.Greater(t, len(ranges), 1, "downloaded in ranges")
	assert.Less(t, len(ranges), 40, "read-ahead must grow for sequential reads")

	// the verification is persisted, and the ranges record is gone
	var verified, partial []byte
	f.db.View(func(tx *bolt.Tx) error {
		verified = tx.Bucket(bucketVerified).Get([]byte(id))
		partial = tx.Bucket(bucketPartial).Get([]byte(id))
		return nil
	})
	assert.NotNil(t, verified)
	assert.Nil(t, partial)

	// reading again uses the cache
	got, err = os.ReadFile(fname)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(content, got))
	assert.EqualValues(t, partialTestSize, mock.DownloadedBytes(id))
}

// Small files are downloaded in full on their first read (not on open).
func TestPartialSmallFileFullDownloadOnRead(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_small"
	content := randomContent(3, 100<<10)
	id := mock.PutFile(remoteDir+"/small.bin", content)
	dir, _ := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "small.bin")

	fd, err := os.Open(fname)
	require.NoError(t, err)
	defer fd.Close()
	assert.Zero(t, mock.DownloadedBytes(id), "open must not download")
	buf := make([]byte, 10)
	_, err = fd.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, content[:10], buf)
	assert.EqualValues(t, len(content), mock.DownloadedBytes(id))
	assert.Equal(t, []string{""}, mock.ContentRanges(id), "one request for the whole file")
	assert.Equal(t, "local", getState(t, fname))
}

// A range served with wrong bytes is caught when the complete file is
// verified, and the file is then downloaded again in full.
func TestPartialCorruptRangeDetected(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_corrupt"
	content := randomContent(4, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	mock.CorruptDownloads(id, 1)
	// the first read sees the corrupted bytes (there is no per-range hash)
	_, err := os.ReadFile(fname)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return getState(t, fname) == "local" },
		10*time.Second, 50*time.Millisecond)
	assert.False(t, f.isPartial(id))
	assert.True(t, f.verifiedNow(id, remoteHash(f, id)))
	assert.GreaterOrEqual(t, mock.DownloadedBytes(id), int64(2*partialTestSize),
		"must have been downloaded again in full")
	cachedContent, ok := cached(f, id)
	require.True(t, ok)
	assert.True(t, cachedContent == string(content), "cached content must be correct")

	got, err := os.ReadFile(fname)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(content, got), "content read after reopening must be correct")
}

// Writing to a partially cached file downloads the rest first, so the upload
// contains the complete content.
func TestPartialWriteDownloadsFirst(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_write"
	content := randomContent(5, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	require.True(t, f.isPartial(id))

	fd, err := os.OpenFile(fname, os.O_RDWR, 0644)
	require.NoError(t, err)
	assert.False(t, f.isPartial(id), "opening for writing must download the rest")
	_, err = fd.WriteAt([]byte("PATCHED"), 5<<20)
	require.NoError(t, err)
	require.NoError(t, fd.Close())

	want := append([]byte(nil), content...)
	copy(want[5<<20:], "PATCHED")
	require.Eventually(t, func() bool {
		data, ok := mock.Content(id)
		return ok && bytes.Equal(data, want)
	}, 20*time.Second, 100*time.Millisecond, "upload must contain the full, patched content")
}

// The same through the Write op directly: a write to a partial file (opened
// read-only before) downloads the rest first.
func TestPartialWriteOpDownloadsFirst(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_writeop"
	content := randomContent(6, 4<<20)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	require.True(t, f.isPartial(id))

	inode := f.GetID(id)
	n, st := f.Write(nil, &fuse.WriteIn{
		InHeader: fuse.InHeader{NodeId: inode.NodeID()},
		Offset:   100,
	}, []byte("xyz"))
	require.Equal(t, fuse.OK, st)
	assert.EqualValues(t, 3, n)
	assert.False(t, f.isPartial(id))
	assert.True(t, inode.HasChanges())

	want := append([]byte(nil), content...)
	copy(want[100:], "xyz")
	got, ok := cached(f, id)
	require.True(t, ok)
	assert.True(t, got == string(want), "cached content must be complete and patched")
	assert.Equal(t, fuse.OK, f.Flush(nil, &fuse.FlushIn{InHeader: fuse.InHeader{NodeId: inode.NodeID()}}))
	require.Eventually(t, func() bool {
		data, ok := mock.Content(id)
		return ok && bytes.Equal(data, want)
	}, 20*time.Second, 100*time.Millisecond, "upload must contain the full, patched content")
}

// A remote change invalidates the cached ranges, whether the file is open or
// not.
func TestPartialRemoteChangeInvalidates(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_remote"
	content := randomContent(7, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	// closed file: the delta drops the ranges
	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	oldHash := remoteHash(f, id)
	v2 := randomContent(8, partialTestSize)
	mock.Modify(id, v2)
	require.Eventually(t, func() bool { return remoteHash(f, id) != oldHash },
		10*time.Second, 50*time.Millisecond)
	got, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	assert.Equal(t, v2[:4096], got)

	// open file: the next read notices the new version
	fd, err := os.Open(fname)
	require.NoError(t, err)
	defer fd.Close()
	buf := make([]byte, 4096)
	_, err = fd.ReadAt(buf, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, v2[1<<20:1<<20+4096], buf)
	hash2 := remoteHash(f, id)
	v3 := randomContent(9, partialTestSize)
	mock.Modify(id, v3)
	require.Eventually(t, func() bool { return remoteHash(f, id) != hash2 },
		10*time.Second, 50*time.Millisecond)
	_, err = fd.ReadAt(buf, 15<<20)
	require.NoError(t, err)
	assert.Equal(t, v3[15<<20:15<<20+4096], buf)
	assert.False(t, cachedRanges(f, id).contains(1<<20, 1<<20+4096),
		"ranges of the old version must be gone")
}

// Concurrent reads of the same chunk fetch it only once.
func TestPartialConcurrentReadsFetchOnce(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_concurrent"
	content := randomContent(10, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, _ := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")
	mock.SetDownloadDelay(id, 300*time.Millisecond)

	base := int64(12 << 20)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := base + int64(i)*16<<10
			fd, err := os.Open(fname)
			if err != nil {
				errs[i] = err
				return
			}
			defer fd.Close()
			buf := make([]byte, 4096)
			if _, err := fd.ReadAt(buf, off); err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(buf, content[off:off+4096]) {
				errs[i] = fmt.Errorf("wrong content at %d", off)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}

	// no byte was requested twice
	var seen rangeSet
	covering := 0
	for _, h := range mock.ContentRanges(id) {
		r := parseRange(t, h)
		assert.Equal(t, rangeSet{r}, seen.missing(r.Start, r.End),
			"range %v was requested twice", r)
		seen = seen.add(r.Start, r.End)
		if r.Start <= base && base < r.End {
			covering++
		}
	}
	assert.Equal(t, 1, covering, "the chunk must be fetched exactly once")
	log.Info().Strs("ranges", mock.ContentRanges(id)).Msg("concurrent read ranges")
}

// While offline, cached ranges stay readable and uncached ones fail.
func TestPartialOffline(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_offline"
	content := randomContent(11, partialTestSize)
	mock.Mkdir(remoteDir)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	cacheDir := filepath.Join(testDBLoc, "e2e_partial_offline")
	mnt, f, unmount := mountMockAtOpts(t, cacheDir, false)
	t.Cleanup(unmount)
	fname := filepath.Join(mnt, remoteDir, "big.bin")

	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)

	f.Lock()
	f.offline = true
	f.Unlock()
	defer func() {
		f.Lock()
		f.offline = false
		f.Unlock()
	}()
	before := mock.DownloadedBytes(id)
	got, err := readAt(t, fname, 200<<10, 4096)
	require.NoError(t, err, "cached range must be readable offline")
	assert.Equal(t, content[200<<10:200<<10+4096], got)
	_, err = readAt(t, fname, 15<<20, 4096)
	assert.Error(t, err, "uncached range must fail offline")
	assert.Equal(t, before, mock.DownloadedBytes(id), "nothing downloaded while offline")
	assert.Equal(t, "cloud", getState(t, fname))
}

// Pinning a partially cached file hydrates it completely.
func TestPartialPinHydrates(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_pin"
	content := randomContent(12, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	require.True(t, f.isPartial(id))

	setPinned(t, fname, true)
	require.Eventually(t, func() bool { return getState(t, fname) == "local" },
		20*time.Second, 100*time.Millisecond)
	assert.False(t, f.isPartial(id))
	assert.True(t, f.verifiedNow(id, remoteHash(f, id)))
	got, ok := cached(f, id)
	require.True(t, ok)
	assert.True(t, got == string(content))
	assert.Equal(t, 0, f.PinStatus().PendingFiles)
}

// Partially cached files can be evicted, and are fetched again afterwards.
func TestPartialEvict(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_evict"
	content := randomContent(13, partialTestSize)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	dir, f := mountMockFS(t, remoteDir)
	fname := filepath.Join(dir, "big.bin")

	_, err := readAt(t, fname, 0, 4096)
	require.NoError(t, err)
	require.True(t, f.isPartial(id))

	require.Eventually(t, func() bool { return evict(fname) == nil },
		5*time.Second, 50*time.Millisecond, "partial file must be evictable once closed")
	_, err = f.content.Stat(id)
	assert.True(t, os.IsNotExist(err), "content must be gone")
	assert.False(t, f.isPartial(id))
	assert.Equal(t, "cloud", getState(t, fname))

	got, err := readAt(t, fname, 3<<20, 4096)
	require.NoError(t, err)
	assert.Equal(t, content[3<<20:3<<20+4096], got)
}

// Cached ranges survive a restart.
func TestPartialRangesPersist(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_partial_persist"
	content := randomContent(14, partialTestSize)
	mock.Mkdir(remoteDir)
	id := mock.PutFile(remoteDir+"/big.bin", content)
	cacheDir := filepath.Join(testDBLoc, "e2e_partial_persist")
	rel := filepath.Join(remoteDir, "big.bin")

	mnt, _, unmount := mountMockAt(t, cacheDir)
	_, err := readAt(t, filepath.Join(mnt, rel), 0, 4096)
	require.NoError(t, err)
	unmount()
	before := mock.DownloadedBytes(id)

	mnt, f, unmount2 := mountMockAt(t, cacheDir)
	defer unmount2()
	assert.True(t, cachedRanges(f, id).contains(0, 4096))
	got, err := readAt(t, filepath.Join(mnt, rel), 0, 4096)
	require.NoError(t, err)
	assert.Equal(t, content[:4096], got)
	assert.Equal(t, before, mock.DownloadedBytes(id), "cached range must not be fetched again")
	assert.Equal(t, "cloud", getState(t, filepath.Join(mnt, rel)))
}
