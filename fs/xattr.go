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

// xattrSpec describes one xattr we expose. A nil set means the attribute is
// read-only. To add a writable attribute (e.g. user.onedriver.pinned), add an
// entry to xattrs/xattrNames with a set func.
type xattrSpec struct {
	get func(f *Filesystem, inode *Inode) []byte
	set func(f *Filesystem, inode *Inode, data []byte) fuse.Status
}

// xattrNames fixes the order attributes are listed in.
var xattrNames = []string{"user.onedriver.state", "user.onedriver.id"}

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
	if !ok {
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

// SetXAttr only accepts writable user.onedriver.* attributes (currently none).
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

// RemoveXAttr is not supported; our attributes are read-only.
func (f *Filesystem) RemoveXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string) fuse.Status {
	if strings.HasPrefix(attr, xattrPrefix) {
		return fuse.EPERM
	}
	return fuse.ENOTSUP
}
