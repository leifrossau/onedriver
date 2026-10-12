package fs

import "sync"

// This file collects the items whose sync state or pin state changed, for the
// FileStatesChanged D-Bus signal. The hot paths only record an ID (O(1)); the
// paths are resolved when the D-Bus service drains the collector, because
// items can be renamed in between. The lock is a leaf lock: nothing is called
// while holding it, and it is never held while taking another lock.

// maxPendingChanges bounds the memory used when nobody drains the collector;
// beyond it, only an "everything changed" flag is kept.
const maxPendingChanges = 20000

type changeSet struct {
	mu       sync.Mutex
	enabled  bool // nothing is recorded until the first take
	ids      map[string]struct{}
	paths    map[string]struct{}
	overflow bool
}

// noteID records that the state of the item with this ID changed.
func (c *changeSet) noteID(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	if c.enabled {
		if len(c.ids)+len(c.paths) >= maxPendingChanges {
			c.overflow = true
		} else {
			if c.ids == nil {
				c.ids = make(map[string]struct{})
			}
			c.ids[id] = struct{}{}
		}
	}
	c.mu.Unlock()
}

// notePath records a change for an item that may be gone by the time the
// collector is drained (deleted items). path is relative to the mountpoint.
func (c *changeSet) notePath(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.enabled {
		if len(c.ids)+len(c.paths) >= maxPendingChanges {
			c.overflow = true
		} else {
			if c.paths == nil {
				c.paths = make(map[string]struct{})
			}
			c.paths[path] = struct{}{}
		}
	}
	c.mu.Unlock()
}

// take returns and clears everything recorded so far, and starts recording if
// it did not yet. overflow means that changes were dropped.
func (c *changeSet) take() (ids, paths []string, overflow bool) {
	c.mu.Lock()
	c.enabled = true
	for id := range c.ids {
		ids = append(ids, id)
	}
	for p := range c.paths {
		paths = append(paths, p)
	}
	overflow = c.overflow
	c.ids, c.paths, c.overflow = nil, nil, false
	c.mu.Unlock()
	return
}

// stateChanged records that the sync or pin state of an item changed.
func (f *Filesystem) stateChanged(id string) {
	f.changes.noteID(id)
}

// TakeChangedPaths returns the paths (relative to the mountpoint, deduplicated)
// of the items whose sync or pin state changed since the last call. If changes
// were dropped, the mount root "" is included. The first call starts the
// recording.
func (f *Filesystem) TakeChangedPaths() []string {
	ids, paths, overflow := f.changes.take()
	seen := make(map[string]struct{}, len(ids)+len(paths))
	out := make([]string, 0, len(ids)+len(paths))
	add := func(p string) {
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, id := range ids {
		if inode := f.GetID(id); inode != nil {
			add(relPath(inode.Path()))
		}
	}
	for _, p := range paths {
		add(p)
	}
	if overflow {
		add("")
	}
	return out
}
