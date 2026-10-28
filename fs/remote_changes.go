package fs

import (
	"sync"

	"github.com/jstaf/onedriver/fs/status"
)

// This file collects the structural changes that came from the server, for
// the KDE KDirNotify signals: FUSE only produces inotify events for changes
// made through the mount, so file managers (KDirWatch) never see items that
// delta polling adds, removes, renames or overwrites. Same rules as
// changeSet: leaf lock, O(1) per event, nothing is recorded until the first
// TakeRemoteChanges (so it costs nothing when nobody consumes it).

// maxPendingRemote bounds the memory used when the collector is not drained;
// beyond it only a "rescan the mount root" marker is kept.
const maxPendingRemote = 20000

type remoteSet struct {
	mu      sync.Mutex
	enabled bool
	events  []status.RemoteChange
	dropped bool
}

func (r *remoteSet) note(c status.RemoteChange) {
	r.mu.Lock()
	if r.enabled {
		if len(r.events) >= maxPendingRemote {
			r.dropped = true
		} else {
			r.events = append(r.events, c)
		}
	}
	r.mu.Unlock()
}

// remoteChange records a change that came from the server. Paths are relative
// to the mountpoint.
func (f *Filesystem) remoteChange(kind, p, newPath string) {
	f.remote.note(status.RemoteChange{Kind: kind, Path: p, NewPath: newPath})
}

// remoteAddedIn records new entries in dir. Folders whose listing is not
// loaded are skipped: nothing could be showing them.
func (f *Filesystem) remoteAddedIn(dir *Inode) {
	if dir == nil || !dir.childrenKnown() {
		return
	}
	f.remoteChange(status.RemoteAdded, relPath(dir.Path()), "")
}

// TakeRemoteChanges returns and clears the server-side changes recorded since
// the last call, and starts the recording on the first call. If events were
// dropped, a RemoteAdded for the mount root is appended.
func (f *Filesystem) TakeRemoteChanges() []status.RemoteChange {
	r := &f.remote
	r.mu.Lock()
	r.enabled = true
	out, dropped := r.events, r.dropped
	r.events, r.dropped = nil, false
	r.mu.Unlock()
	if dropped {
		out = append(out, status.RemoteChange{Kind: status.RemoteAdded})
	}
	return out
}
