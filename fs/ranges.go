package fs

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

// Bookkeeping for partially downloaded files (see partial.go).
//
// A partially cached file has a sparse cache file, pre-sized to the item's
// size, and a record of the byte ranges of it that were downloaded. The record
// belongs to one version of the item (its QuickXorHash): when the remote
// version changes, the ranges are worthless and are dropped. Records are kept
// in memory (the number of partial files is small) and persisted in the
// "partial" bolt bucket, so cached ranges survive a restart. A file without a
// record is either fully cached or not cached at all.

var bucketPartial = []byte("partial")

// interval is the half-open byte range [Start, End). It is stored as a two
// element JSON array.
type interval struct {
	Start, End int64
}

func (iv interval) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int64{iv.Start, iv.End})
}

func (iv *interval) UnmarshalJSON(data []byte) error {
	var a [2]int64
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	iv.Start, iv.End = a[0], a[1]
	return nil
}

// rangeSet is a sorted list of non-overlapping, non-adjacent intervals.
type rangeSet []interval

// add returns the set with [start, end) added, merging intervals that
// overlap or touch it.
func (s rangeSet) add(start, end int64) rangeSet {
	if end <= start {
		return s
	}
	out := make(rangeSet, 0, len(s)+1)
	i := 0
	for ; i < len(s) && s[i].End < start; i++ {
		out = append(out, s[i])
	}
	for ; i < len(s) && s[i].Start <= end; i++ {
		if s[i].Start < start {
			start = s[i].Start
		}
		if s[i].End > end {
			end = s[i].End
		}
	}
	out = append(out, interval{start, end})
	return append(out, s[i:]...)
}

// contains returns true if all of [start, end) is in the set.
func (s rangeSet) contains(start, end int64) bool {
	if end <= start {
		return true
	}
	for _, iv := range s {
		if iv.Start <= start && end <= iv.End {
			return true
		}
		if iv.Start > start {
			break
		}
	}
	return false
}

// missing returns the parts of [start, end) that are not in the set.
func (s rangeSet) missing(start, end int64) rangeSet {
	var out rangeSet
	for _, iv := range s {
		if start >= end {
			break
		}
		if iv.End <= start {
			continue
		}
		if iv.Start >= end {
			break
		}
		if iv.Start > start {
			out = append(out, interval{start, iv.Start})
		}
		start = iv.End
	}
	if start < end {
		out = append(out, interval{start, end})
	}
	return out
}

// total returns the number of bytes in the set.
func (s rangeSet) total() int64 {
	var n int64
	for _, iv := range s {
		n += iv.End - iv.Start
	}
	return n
}

// partialRecord is the persisted form of a partial file.
type partialRecord struct {
	Hash   string   `json:"hash"`
	Size   int64    `json:"size"`
	Ranges rangeSet `json:"ranges"`
}

// partialFile is the state of one partially cached file. All fields are
// protected by mu. Lock order: inode lock -> partialFile.mu. Nothing that
// takes the content cache's locks is called while holding mu.
type partialFile struct {
	mu     sync.Mutex
	hash   string // lower-case QuickXorHash of the version the ranges belong to
	size   int64
	ranges rangeSet
	// gen changes whenever the ranges are reset, so fetches started before
	// that do not record their (stale) data
	gen uint64
	// dead is set when the file stops being partial (fully downloaded,
	// deleted or evicted); a dead partialFile is never used again
	dead bool
	// chunks being fetched, by chunk index; closed when done (or failed)
	inflight map[int64]chan struct{}
	// end of the previous read and the current read-ahead, see partial.go
	nextOff   int64
	readahead int64
}

func (p *partialFile) record() partialRecord {
	return partialRecord{Hash: p.hash, Size: p.size, Ranges: p.ranges}
}

// loadPartials reads the partial file records from the database. Records
// whose content file is gone are dropped.
func (f *Filesystem) loadPartials() {
	var stale []string
	f.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPartial)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			id := string(k)
			var rec partialRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				stale = append(stale, id)
				return nil
			}
			if st, err := f.content.Stat(id); err != nil || st.Size() != rec.Size {
				stale = append(stale, id)
				return nil
			}
			f.partials.Store(id, &partialFile{
				hash:     rec.Hash,
				size:     rec.Size,
				ranges:   rec.Ranges,
				inflight: make(map[int64]chan struct{}),
				nextOff:  -1,
			})
			return nil
		})
	})
	for _, id := range stale {
		f.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketPartial).Delete([]byte(id))
		})
		// without its record, the content cannot be trusted
		f.content.Delete(id)
	}
}

// partialGet returns the partial state of an ID, or nil if the content is
// not partial (fully cached or not cached at all).
func (f *Filesystem) partialGet(id string) *partialFile {
	if v, ok := f.partials.Load(id); ok {
		return v.(*partialFile)
	}
	return nil
}

// isPartial returns true if only parts of an item's content are cached.
func (f *Filesystem) isPartial(id string) bool {
	return f.partialGet(id) != nil
}

// hasFullContent returns true if the complete content of an item is cached
// (which does not imply it is up to date).
func (f *Filesystem) hasFullContent(id string) bool {
	return f.content.HasContent(id) && !f.isPartial(id)
}

// persistPartial writes the record of p to the database. The caller holds
// p.mu, so records of one file are written in order.
func (f *Filesystem) persistPartial(id string, p *partialFile) {
	if f.db == nil {
		return
	}
	data, err := json.Marshal(p.record())
	if err != nil {
		return
	}
	err = f.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketPartial)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
	if err != nil {
		log.Error().Err(err).Str("id", id).Msg("Could not save cached ranges.")
	}
}

// partialStart makes the content of id partial with no ranges cached, for the
// version with the given hash and size: the cache file is truncated to an
// empty sparse file of that size. An existing partial state is reset.
func (f *Filesystem) partialStart(id, hash string, size int64) (*partialFile, error) {
	for {
		fresh := &partialFile{inflight: make(map[int64]chan struct{}), nextOff: -1}
		v, _ := f.partials.LoadOrStore(id, fresh)
		p := v.(*partialFile)
		p.mu.Lock()
		if p.dead {
			// lost a race against partialForget, start over with a new one
			p.mu.Unlock()
			f.partials.CompareAndDelete(id, p)
			continue
		}
		if err := truncateSparse(f.content.contentPath(id), size); err != nil {
			p.dead = true
			p.mu.Unlock()
			f.partials.CompareAndDelete(id, p)
			return nil, err
		}
		p.gen++
		p.hash = strings.ToLower(hash)
		p.size = size
		p.ranges = nil
		p.nextOff, p.readahead = -1, 0
		f.persistPartial(id, p)
		p.mu.Unlock()
		return p, nil
	}
}

// truncateSparse empties a file and resizes it to size without allocating
// any blocks.
func truncateSparse(path string, size int64) error {
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	return os.Truncate(path, size)
}

// partialForget drops the partial state of id (the content became complete,
// or it was deleted or evicted). Fetches still running discard their data.
func (f *Filesystem) partialForget(id string) {
	v, ok := f.partials.LoadAndDelete(id)
	if !ok {
		return
	}
	p := v.(*partialFile)
	p.mu.Lock()
	p.dead = true
	p.gen++
	p.mu.Unlock()
	f.partialDropRecord(id)
}

// partialDropRecord deletes the persisted record of a partial file.
func (f *Filesystem) partialDropRecord(id string) {
	if f.db == nil || strings.HasPrefix(id, "temp-") {
		return
	}
	f.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketPartial); b != nil {
			return b.Delete([]byte(id))
		}
		return nil
	})
}

// diskUsage returns the space a cache file takes up on disk: partially
// downloaded files are sparse, so their size overstates it.
func diskUsage(info os.FileInfo) uint64 {
	size := uint64(info.Size())
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if used := uint64(st.Blocks) * 512; used < size {
			return used
		}
	}
	return size
}
