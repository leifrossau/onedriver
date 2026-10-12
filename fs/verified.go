package fs

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	bolt "go.etcd.io/bbolt"
)

// Persistent record of which cached content files were hash-verified, so that
// the first Open after a restart does not have to re-hash a large file.
//
// A cached file is trusted without hashing only if the stored hash equals the
// item's current remote hash AND the file's size and mtime (nanosecond
// precision) are exactly what they were when the hash was verified. Any write
// through FUSE (or behind our back) changes the mtime, so stale records are
// never trusted. Records are also dropped when content is deleted or evicted.

var bucketVerified = []byte("verified")

// verifiedContent records the state of a cache file when its hash was last
// verified, so unchanged files need not be re-hashed on every open.
type verifiedContent struct {
	hash  string
	size  int64
	mtime time.Time
}

type verifiedRecord struct {
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtimeNs"` // unix nanoseconds
}

// newVerified describes the cache file with the given stat result.
func newVerified(hash string, st os.FileInfo) verifiedContent {
	return verifiedContent{hash: strings.ToLower(hash), size: st.Size(), mtime: st.ModTime()}
}

// same returns true if both describe the same hash and file state.
func (v verifiedContent) same(o verifiedContent) bool {
	return v.hash == o.hash && v.size == o.size && v.mtime.UnixNano() == o.mtime.UnixNano()
}

// verifiedGet returns the verification record of an ID, loading it lazily
// from the database.
func (f *Filesystem) verifiedGet(id string) (verifiedContent, bool) {
	if v, ok := f.verified.Load(id); ok {
		return v.(verifiedContent), true
	}
	if f.db == nil {
		return verifiedContent{}, false
	}
	var rec verifiedRecord
	found := false
	f.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVerified)
		if b == nil {
			return nil
		}
		if data := b.Get([]byte(id)); data != nil {
			found = json.Unmarshal(data, &rec) == nil
		}
		return nil
	})
	if !found {
		return verifiedContent{}, false
	}
	v := verifiedContent{hash: rec.Hash, size: rec.Size, mtime: time.Unix(0, rec.MTime)}
	f.verified.Store(id, v)
	return v, true
}

// verifiedPut remembers (in memory and on disk) that the content of id is
// verified.
func (f *Filesystem) verifiedPut(id string, v verifiedContent) {
	f.verified.Store(id, v)
	if f.db == nil {
		return
	}
	data, err := json.Marshal(verifiedRecord{Hash: v.hash, Size: v.size, MTime: v.mtime.UnixNano()})
	if err != nil {
		return
	}
	f.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketVerified)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
}

// verifiedForget drops the verification record of an ID.
func (f *Filesystem) verifiedForget(id string) {
	f.verified.Delete(id)
	if f.db == nil || strings.HasPrefix(id, "temp-") {
		return
	}
	f.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVerified)
		if b == nil || b.Get([]byte(id)) == nil {
			return nil
		}
		return b.Delete([]byte(id))
	})
}

// hashContent computes the QuickXorHash of a file and counts how often it was
// done (tests use this to check that verified files are not re-hashed).
func (f *Filesystem) hashContent(r io.ReadSeeker) string {
	f.hashCount.Add(1)
	return graph.QuickXORHashStream(r)
}
