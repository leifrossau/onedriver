package fs

import (
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// SyncState describes where a file is in its sync lifecycle. It is exposed to
// userspace (file manager overlays, scripts) through the user.onedriver.state
// extended attribute.
type SyncState string

const (
	// SyncCloud means the content has not been downloaded.
	SyncCloud SyncState = "cloud"
	// SyncLocal means the content is cached locally and in sync.
	SyncLocal SyncState = "local"
	// SyncModified means local edits exist that have not been queued for upload.
	SyncModified SyncState = "modified"
	// SyncUploading means an upload is pending or in progress.
	SyncUploading SyncState = "uploading"
	// SyncError means the upload session is in an error state.
	SyncError SyncState = "error"
	// SyncConflict is reserved for future use.
	SyncConflict SyncState = "conflict"
)

const xattrPrefix = "user.onedriver."

// xattrSpec describes one xattr we expose. A nil get means the attribute is
// write-only (it is then not listed either), a nil set means it is read-only.
// remove is called by removexattr, which is refused if it is nil.
type xattrSpec struct {
	get    func(f *Filesystem, inode *Inode) []byte
	set    func(f *Filesystem, inode *Inode, data []byte) fuse.Status
	remove func(f *Filesystem, inode *Inode) fuse.Status
}

// xattrNames fixes the order attributes are listed in.
var xattrNames = []string{"user.onedriver.state", "user.onedriver.id", "user.onedriver.pinned"}

var xattrs = map[string]xattrSpec{
	"user.onedriver.state": {
		get: func(f *Filesystem, inode *Inode) []byte {
			return []byte(f.syncState(inode))
		},
	},
	"user.onedriver.id": {
		get: func(f *Filesystem, inode *Inode) []byte {
			inode.RLock()
			defer inode.RUnlock()
			return []byte(inode.DriveItem.ID)
		},
	},
	// "1" if the item is kept on this device (pinned itself or inside a
	// pinned folder), "0" otherwise. Writing 1/0 pins/unpins the item (for
	// folders: with everything inside them), removing the attribute unpins.
	// Only records the state, downloads happen in the background.
	"user.onedriver.pinned": {
		get: func(f *Filesystem, inode *Inode) []byte {
			if f.isPinned(inode) {
				return []byte("1")
			}
			return []byte("0")
		},
		set: func(f *Filesystem, inode *Inode, data []byte) fuse.Status {
			pinned, ok := parseBoolXAttr(data)
			if !ok {
				return fuse.EINVAL
			}
			f.setPinned(inode, pinned)
			return fuse.OK
		},
		remove: func(f *Filesystem, inode *Inode) fuse.Status {
			f.setPinned(inode, false)
			return fuse.OK
		},
	},
	// Writing any value frees up the space used by the item's cached content
	// (for folders: of everything inside them). Pinned, open or modified
	// files are skipped; for a single file that cannot be evicted, EBUSY is
	// returned.
	"user.onedriver.evict": {
		set: func(f *Filesystem, inode *Inode, data []byte) fuse.Status {
			if _, err := f.EvictContent(inode.ID()); err != nil {
				return fuse.EBUSY
			}
			return fuse.OK
		},
	},
}

// parseBoolXAttr parses the value written to a boolean attribute.
func parseBoolXAttr(data []byte) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(strings.TrimRight(string(data), "\x00"))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off", "":
		return false, true
	}
	return false, false
}

// combineSyncState is the pure logic that merges the three sources of sync
// state: pending local edits, the upload manager and the content cache.
// Precedence: error > modified > uploading > local/cloud. Directories are
// always reported as local.
func combineSyncState(isDir, hasChanges, hasSession bool, uploadState int, hasContent bool) SyncState {
	if isDir {
		return SyncLocal
	}
	if hasSession && uploadState == uploadErrored {
		return SyncError
	}
	if hasChanges {
		return SyncModified
	}
	if hasSession && (uploadState == uploadNotStarted || uploadState == uploadStarted) {
		return SyncUploading
	}
	if hasContent {
		return SyncLocal
	}
	return SyncCloud
}

func (f *Filesystem) syncState(inode *Inode) SyncState {
	if inode.IsDir() {
		return SyncLocal
	}
	inode.RLock()
	id := inode.DriveItem.ID
	inode.RUnlock()

	var state int
	var hasSession bool
	if f.uploads != nil {
		state, hasSession = f.uploads.sessionState(id)
	}
	return combineSyncState(false, inode.HasChanges(), hasSession, state, f.content.HasContent(id))
}

// SyncState returns the sync state of the item with the given ID. Unknown IDs
// are reported as cloud.
func (f *Filesystem) SyncState(id string) SyncState {
	inode := f.GetID(id)
	if inode == nil {
		return SyncCloud
	}
	return f.syncState(inode)
}

// GetXAttr reads an extended attribute.
func (f *Filesystem) GetXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string, dest []byte) (uint32, fuse.Status) {
	spec, ok := xattrs[attr]
	if !ok || spec.get == nil {
		return 0, fuse.ENOATTR
	}
	inode := f.GetNodeID(header.NodeId)
	if inode == nil {
		return 0, fuse.ENOENT
	}
	return fillXAttr(spec.get(f, inode), dest)
}

// ListXAttr lists our extended attributes as a NUL-separated list.
func (f *Filesystem) ListXAttr(cancel <-chan struct{}, header *fuse.InHeader, dest []byte) (uint32, fuse.Status) {
	if f.GetNodeID(header.NodeId) == nil {
		return 0, fuse.ENOENT
	}
	var list []byte
	for _, name := range xattrNames {
		list = append(list, name...)
		list = append(list, 0)
	}
	return fillXAttr(list, dest)
}

// fillXAttr copies value into dest. If dest is too small (including the
// size-0 probe), ERANGE is returned along with the required size.
func fillXAttr(value, dest []byte) (uint32, fuse.Status) {
	if len(dest) < len(value) {
		return uint32(len(value)), fuse.ERANGE
	}
	return uint32(copy(dest, value)), fuse.OK
}

// SetXAttr only accepts the writable user.onedriver.* attributes
// (user.onedriver.pinned and user.onedriver.evict).
func (f *Filesystem) SetXAttr(cancel <-chan struct{}, input *fuse.SetXAttrIn, attr string, data []byte) fuse.Status {
	if !strings.HasPrefix(attr, xattrPrefix) {
		return fuse.ENOTSUP
	}
	spec, ok := xattrs[attr]
	if !ok || spec.set == nil {
		return fuse.EPERM
	}
	inode := f.GetNodeID(input.NodeId)
	if inode == nil {
		return fuse.ENOENT
	}
	return spec.set(f, inode, data)
}

// RemoveXAttr is only supported for user.onedriver.pinned (it unpins).
func (f *Filesystem) RemoveXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string) fuse.Status {
	if !strings.HasPrefix(attr, xattrPrefix) {
		return fuse.ENOTSUP
	}
	spec, ok := xattrs[attr]
	if !ok || spec.remove == nil {
		return fuse.EPERM
	}
	inode := f.GetNodeID(header.NodeId)
	if inode == nil {
		return fuse.ENOENT
	}
	return spec.remove(f, inode)
}
