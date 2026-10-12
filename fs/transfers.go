package fs

import (
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/status"
)

// This file tracks running and queued transfers for the GetTransfers D-Bus
// method. The registry lock is a leaf lock: nothing is called while holding
// it, and it is never held while taking another lock.

// maxQueuedPins caps how many pinned files waiting for hydration are listed.
const maxQueuedPins = 200

type transferKey struct {
	id  string
	dir string
}

type transferEntry struct {
	name     string // fallback if the item is gone when listing
	state    string
	done     uint64
	total    uint64
	queuedAt time.Time
}

// transferRegistry is the set of known transfers, keyed by item ID and
// direction. The zero value is ready to use; methods on a nil registry do
// nothing.
type transferRegistry struct {
	mu      sync.Mutex
	entries map[transferKey]*transferEntry
	version atomic.Uint64
}

// Version changes whenever the listed transfers may have changed.
func (r *transferRegistry) Version() uint64 {
	if r == nil {
		return 0
	}
	return r.version.Load()
}

// touch marks the listing as changed without modifying an entry.
func (r *transferRegistry) touch() {
	if r != nil {
		r.version.Add(1)
	}
}

// update modifies an entry, creating it with create if it does not exist
// (create may be nil to only modify existing entries).
func (r *transferRegistry) update(id, dir string, create func() *transferEntry, fn func(*transferEntry)) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	if r.entries == nil {
		r.entries = make(map[transferKey]*transferEntry)
	}
	e := r.entries[transferKey{id, dir}]
	if e == nil {
		if create == nil {
			r.mu.Unlock()
			return
		}
		e = create()
		r.entries[transferKey{id, dir}] = e
	}
	if fn != nil {
		fn(e)
	}
	r.mu.Unlock()
	r.version.Add(1)
}

// queue registers a transfer as queued, or updates name and total of a known
// one.
func (r *transferRegistry) queue(id, dir, name string, total uint64) {
	r.update(id, dir, func() *transferEntry {
		return &transferEntry{state: status.TransferQueued, queuedAt: time.Now()}
	}, func(e *transferEntry) {
		if name != "" {
			e.name = name
		}
		if total > 0 {
			e.total = total
		}
	})
}

// setState changes the state of an existing entry and resets its progress.
func (r *transferRegistry) setState(id, dir, state string) {
	r.update(id, dir, nil, func(e *transferEntry) {
		e.state = state
		e.done = 0
	})
}

// setDone records the progress of an existing entry.
func (r *transferRegistry) setDone(id, dir string, done uint64) {
	r.update(id, dir, nil, func(e *transferEntry) { e.done = done })
}

// remove forgets a transfer.
func (r *transferRegistry) remove(id, dir string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	_, ok := r.entries[transferKey{id, dir}]
	delete(r.entries, transferKey{id, dir})
	r.mu.Unlock()
	if ok {
		r.version.Add(1)
	}
}

// rekey moves an entry to a new ID (local ID replaced by the server ID).
func (r *transferRegistry) rekey(oldID, newID, dir string) {
	if r == nil || oldID == newID {
		return
	}
	r.mu.Lock()
	e, ok := r.entries[transferKey{oldID, dir}]
	if ok {
		delete(r.entries, transferKey{oldID, dir})
		if _, taken := r.entries[transferKey{newID, dir}]; !taken {
			r.entries[transferKey{newID, dir}] = e
		}
	}
	r.mu.Unlock()
	if ok {
		r.version.Add(1)
	}
}

// transferItem is a snapshot of an entry, with its ID.
type transferItem struct {
	id  string
	dir string
	transferEntry
}

// snapshot copies all entries.
func (r *transferRegistry) snapshot() []transferItem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]transferItem, 0, len(r.entries))
	for k, e := range r.entries {
		out = append(out, transferItem{k.id, k.dir, *e})
	}
	return out
}

// countingWriter reports how many bytes were written through it.
type countingWriter struct {
	w      io.Writer
	n      uint64
	report func(done uint64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += uint64(n)
	if n > 0 && c.report != nil {
		c.report(c.n)
	}
	return n, err
}

// Transfers lists the uploads and downloads that are queued, running or
// waiting to be retried, active ones first, then by queue time. Paths are
// relative to the mountpoint and computed now, as items may have been
// renamed. No locks are held while inodes are inspected.
func (f *Filesystem) Transfers() []status.Transfer {
	items := f.transfers.snapshot()
	known := make(map[string]bool)
	for _, it := range items {
		if it.dir == status.DirDownload {
			known[it.id] = true
		}
	}

	// pinned files waiting for hydration, capped
	if f.pins != nil {
		type pend struct {
			id       string
			attempts int
			at       time.Time
		}
		var pending []pend
		f.pins.mu.Lock()
		for id, job := range f.pins.pending {
			if len(pending) >= 4*maxQueuedPins {
				break
			}
			if !known[id] && !isLocalID(id) {
				pending = append(pending, pend{id, job.attempts, job.queuedAt})
			}
		}
		f.pins.mu.Unlock()
		added := 0
		for _, p := range pending {
			if added >= maxQueuedPins {
				break
			}
			inode := f.GetID(p.id)
			if inode == nil {
				continue
			}
			inode.RLock()
			dir := inode.DriveItem.IsDir() || inode.mode&fuse.S_IFDIR > 0
			size := inode.DriveItem.Size
			hasChanges := inode.hasChanges
			hash := ""
			if inode.DriveItem.File != nil {
				hash = inode.DriveItem.File.Hashes.QuickXorHash
			}
			inode.RUnlock()
			if dir || hasChanges || f.verifiedNow(p.id, hash) {
				continue
			}
			state := status.TransferQueued
			if p.attempts > 0 {
				state = status.TransferRetrying
			}
			added++
			items = append(items, transferItem{id: p.id, dir: status.DirDownload,
				transferEntry: transferEntry{state: state, total: size, queuedAt: p.at}})
		}
	}

	type row struct {
		t  status.Transfer
		at time.Time
	}
	rows := make([]row, 0, len(items))
	for _, it := range items {
		name := it.name
		if inode := f.GetID(it.id); inode != nil {
			name = inode.Path()
		}
		if name == "" {
			continue
		}
		rows = append(rows, row{status.Transfer{
			Path: relPath(name), Direction: it.dir, State: it.state,
			BytesDone: it.done, BytesTotal: it.total, QueuedAt: it.queuedAt.Unix(),
		}, it.queuedAt})
	}
	sort.SliceStable(rows, func(a, b int) bool {
		aa, ab := rows[a].t.State == status.TransferActive, rows[b].t.State == status.TransferActive
		if aa != ab {
			return aa
		}
		return rows[a].at.Before(rows[b].at)
	})
	out := make([]status.Transfer, len(rows))
	for i, r := range rows {
		out[i] = r.t
	}
	return out
}

// TransfersVersion changes whenever the result of Transfers may have changed.
func (f *Filesystem) TransfersVersion() uint64 {
	return f.transfers.Version()
}
