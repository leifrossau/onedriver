package dbus

import (
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog/log"
)

// KDE's KDirNotify: KCoreDirLister (Dolphin, KIO file dialogs) listens for
// these signals from any sender to learn about changes it cannot see through
// inotify. FUSE only reports inotify events for changes made through the
// mount, so items that delta polling adds, removes, renames or overwrites
// would stay invisible until the user refreshes. We emit them the way
// KDirNotify::emitFilesAdded & co. do (KIO, src/core/kdirnotify.cpp): signals
// on object path "/", interface "org.kde.KDirNotify", on the session bus, with
// file:// URL strings.
const (
	KDirNotifyPath  = dbus.ObjectPath("/")
	KDirNotifyIface = "org.kde.KDirNotify"

	// kdirInterval is how often collected changes are flushed
	kdirInterval = 500 * time.Millisecond

	maxKDirParents = 200 // FilesAdded signals per flush
	maxKDirRemoved = 500 // URLs in one FilesRemoved
	maxKDirRenamed = 200 // FileRenamed pairs per flush
	maxKDirChanged = 500 // URLs in one FilesChanged
)

// RemoteBackend is implemented by backends that collect server-side changes.
type RemoteBackend interface {
	// TakeRemoteChanges returns and clears the changes since the last call
	TakeRemoteChanges() []status.RemoteChange
}

// KDirSignals are the coalesced KDirNotify signals for a batch of changes.
type KDirSignals struct {
	Added   []string    // FilesAdded(dir URL), one signal each
	Removed []string    // FilesRemoved(urls)
	Renamed [][2]string // FileRenamed(src, dst)
	Changed []string    // FilesChanged(urls)
}

// Empty reports whether there is nothing to send.
func (k KDirSignals) Empty() bool {
	return len(k.Added) == 0 && len(k.Removed) == 0 && len(k.Renamed) == 0 && len(k.Changed) == 0
}

func parentOf(p string) string {
	d := filepath.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// CoalesceRemote turns server-side changes into KDirNotify signals:
// FilesAdded once per affected folder, and the removed / renamed / changed
// lists deduplicated and capped. When a list is too long, FilesAdded for the
// affected parent folders replaces it (a lister rescans the folder and so
// finds removed, renamed and changed entries as well). When there are too many
// folders, they are replaced by their parents, and finally the mount root.
func CoalesceRemote(mountpoint string, changes []status.RemoteChange) KDirSignals {
	var out KDirSignals
	added := make(map[string]struct{})
	var addedOrder []string
	addDir := func(d string) {
		if _, dup := added[d]; !dup {
			added[d] = struct{}{}
			addedOrder = append(addedOrder, d)
		}
	}
	removedSet := make(map[string]struct{})
	changedSet := make(map[string]struct{})
	renamedSet := make(map[[2]string]struct{})
	var removed, changed []string
	var renamed [][2]string
	for _, c := range changes {
		switch c.Kind {
		case status.RemoteAdded:
			addDir(c.Path)
		case status.RemoteRemoved:
			if _, dup := removedSet[c.Path]; !dup {
				removedSet[c.Path] = struct{}{}
				removed = append(removed, c.Path)
			}
		case status.RemoteRenamed:
			k := [2]string{c.Path, c.NewPath}
			if _, dup := renamedSet[k]; !dup {
				renamedSet[k] = struct{}{}
				renamed = append(renamed, k)
			}
		case status.RemoteChanged:
			if _, dup := changedSet[c.Path]; !dup {
				changedSet[c.Path] = struct{}{}
				changed = append(changed, c.Path)
			}
		}
	}
	// a file that is gone needs no refresh
	kept := changed[:0]
	for _, p := range changed {
		if _, gone := removedSet[p]; !gone {
			kept = append(kept, p)
		}
	}
	changed = kept

	if len(removed) > maxKDirRemoved {
		for _, p := range removed {
			addDir(parentOf(p))
		}
		removed = nil
	}
	if len(renamed) > maxKDirRenamed {
		for _, r := range renamed {
			addDir(parentOf(r[0]))
			addDir(parentOf(r[1]))
		}
		renamed = nil
	}
	if len(changed) > maxKDirChanged {
		for _, p := range changed {
			addDir(parentOf(p))
		}
		changed = nil
	}
	dirs := addedOrder
	if len(dirs) > maxKDirParents {
		dirs = CollapsePaths(dirs, maxKDirParents)
	}

	for _, d := range dirs {
		out.Added = append(out.Added, FileURL(mountpoint, d))
	}
	for _, p := range removed {
		out.Removed = append(out.Removed, FileURL(mountpoint, p))
	}
	for _, r := range renamed {
		out.Renamed = append(out.Renamed, [2]string{FileURL(mountpoint, r[0]), FileURL(mountpoint, r[1])})
	}
	for _, p := range changed {
		out.Changed = append(out.Changed, FileURL(mountpoint, p))
	}
	return out
}

// flushKDirNotify emits the collected server-side changes.
func (s *Service) flushKDirNotify() {
	changes := s.remote.TakeRemoteChanges()
	if len(changes) == 0 {
		return
	}
	sig := CoalesceRemote(s.mountpoint, changes)
	emit := func(name string, args ...interface{}) {
		if err := s.conn.Emit(KDirNotifyPath, KDirNotifyIface+"."+name, args...); err != nil {
			log.Debug().Err(err).Str("signal", name).Msg("Could not emit KDirNotify signal.")
		}
	}
	if len(sig.Removed) > 0 {
		emit("FilesRemoved", sig.Removed)
	}
	for _, r := range sig.Renamed {
		// KIO sends both, listeners use whichever they know
		emit("FileRenamed", r[0], r[1])
		emit("FileRenamedWithLocalPath", r[0], r[1], "")
	}
	for _, d := range sig.Added {
		emit("FilesAdded", d)
	}
	if len(sig.Changed) > 0 {
		emit("FilesChanged", sig.Changed)
	}
}

// Option configures a Service.
type Option func(*Service)

// WithoutKDirNotify turns the KDirNotify signals off.
func WithoutKDirNotify() Option { return func(s *Service) { s.remote = nil } }
