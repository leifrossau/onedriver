package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jstaf/onedriver/fs/conflict"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

// DeltaLoop creates a new thread to poll the server for changes and should be
// called as a goroutine
func (f *Filesystem) DeltaLoop(interval time.Duration) {
	log.Trace().Msg("Starting delta goroutine.")
	if !f.startLoop() {
		return
	}
	defer f.loops.Done()
	// lastPoll deliberately has no monotonic reading (see below), failures
	// counts consecutive failed polls for the retry backoff.
	lastPoll := time.Now().Round(0)
	failures := 0
	for { // eva
		// no polling while paused, Resume and SyncNow wake us up
		if f.Paused() {
			f.cancelCheck()
			if !f.sleepOrWake(time.Second) {
				return
			}
			continue
		}

		// no requests without a login, Login and SyncNow wake us up
		if f.AuthExpired() {
			f.endCheck(f.checkTicket()) // nothing can be polled
			if !f.sleepOrWake(10 * time.Second) {
				return
			}
			continue
		}

		// After a suspend the keep-alive connection from the last poll is dead,
		// but the kernel does not know yet: the request would hang until the
		// client timeout (a minute) and SyncNow could not help. The monotonic
		// clock does not advance while suspended, the wall clock does, so a
		// large wall-clock gap means we slept through something.
		if time.Now().Round(0).Sub(lastPoll) > 2*interval+30*time.Second {
			log.Info().Msg("Long gap since the last delta poll (suspend?), " +
				"dropping idle connections.")
			http.DefaultClient.CloseIdleConnections()
		}

		// A requested resync starts over with the complete enumeration. The
		// request stays pending until a poll that began at the start
		// succeeded, a failure halfway must not leave a half-done resync.
		ticket := f.checkTicket()
		fullEnum := f.resyncRequested()
		if fullEnum {
			log.Info().Msg("Resync requested, fetching a full delta.")
			f.deltaLink = fullDeltaLink
		}

		// get deltas
		log.Trace().Msg("Fetching deltas from server.")
		pollSuccess := false
		deltas := make(map[string]*graph.DriveItem)
		for {
			incoming, cont, err := f.pollDeltas(f.auth)
			if errors.Is(err, graph.ErrAuthExpired) {
				// not the network's fault, the next round waits for the login
				log.Debug().Msg("Delta fetch stopped, login expired.")
				break
			}
			if graph.IsResyncRequired(err) && f.deltaLink != fullDeltaLink {
				// The server forgot our delta token (410 Gone / resyncRequired),
				// which happens after a long time without polling. Retrying the
				// same link would fail forever, so start over with a complete
				// enumeration. It reports every item's current state, so new
				// and changed items are picked up (items deleted meanwhile are
				// not reported, those linger until the folder is re-listed). A
				// 410 on the full link itself is treated like any other error.
				log.Warn().Err(err).
					Msg("Delta token expired, resyncing from a full delta.")
				f.deltaLink = fullDeltaLink
				fullEnum = false // not a user resync, do not remove anything
				continue
			}
			if err != nil {
				// the only thing that should be able to bring the FS out
				// of a read-only state is a successful delta call
				log.Error().Err(err).
					Msg("Error during delta fetch, marking fs as offline.")
				f.Lock()
				f.offline = true
				f.Unlock()
				break
			}

			for _, delta := range incoming {
				// As per the API docs, the last delta received from the server
				// for an item is the one we should use.
				deltas[delta.ID] = delta
			}
			if !cont {
				log.Info().Msgf("Fetched %d deltas.", len(deltas))
				pollSuccess = true
				break
			}
		}

		// now apply deltas
		f.applyDeltas(deltas)

		if fullEnum && pollSuccess && !f.IsOffline() {
			f.removeStale(deltas)
			f.finishResync()
		}

		if !f.IsOffline() {
			f.SerializeAll()
		}

		lastPoll = time.Now().Round(0)
		if pollSuccess {
			failures = 0
			f.Lock()
			if f.offline {
				log.Info().Msg("Delta fetch success, marking fs as online.")
			}
			f.offline = false
			f.Unlock()
			f.noteSyncSuccess()
			f.endCheck(ticket)

			// never persist an unusable link, it would break the next startup
			if f.deltaLink != "" {
				f.db.Batch(func(tx *bolt.Tx) error {
					return tx.Bucket(bucketDelta).Put([]byte("deltaLink"), []byte(f.deltaLink))
				})
			}

			// wait until next interval (or until SyncNow)
			if !f.sleepOrWake(interval) {
				return
			}
		} else {
			// Shortened duration while offline, backing off so a throttled
			// (429) or flapping server is not hammered. SyncNow cuts the wait
			// short, so "Sync now" always retries right away.
			f.endCheck(ticket)
			failures++
			shift := failures - 1
			if shift > 4 {
				shift = 4
			}
			wait := 2 * time.Second << uint(shift) // 2s .. 32s
			if !f.sleepOrWake(wait) {
				return
			}
		}
	}
}

// errParentMissing is returned by applyDelta for items whose parent folder is
// not in the cache (yet).
var errParentMissing = errors.New("parent not in cache")

// errDirNotEmpty is returned for a delta deleting a folder that still has
// children.
var errDirNotEmpty = errors.New("directory is non-empty")

// applyDeltas applies the deltas of one polling cycle. The Graph API does not
// promise that a folder is listed before the items inside it (neither within a
// page nor across pages), so the order is not trusted: items whose parent
// folder is not in the cache are retried after the rest has been applied (the
// parent may be one of the deltas), until a pass makes no more progress. The
// same goes for deletions of folders that are not empty yet. Items whose
// parent is genuinely unknown (a folder that was never loaded) stay skipped,
// those get fetched from the server when the folder is first listed.
func (f *Filesystem) applyDeltas(deltas map[string]*graph.DriveItem) {
	// deterministic order: the folders first, then by ID, which makes most
	// items succeed in the first pass
	items := make([]*graph.DriveItem, 0, len(deltas))
	for _, d := range deltas {
		items = append(items, d)
	}
	sort.Slice(items, func(i, j int) bool {
		di, dj := items[i].IsDir() && items[i].Deleted == nil, items[j].IsDir() && items[j].Deleted == nil
		if di != dj {
			return di
		}
		return items[i].ID < items[j].ID
	})

	pending := items
	firstPass := true
	retried := 0
	for len(pending) > 0 {
		var again []*graph.DriveItem
		for _, d := range pending {
			err := f.applyDelta(d)
			if errors.Is(err, errParentMissing) || errors.Is(err, errDirNotEmpty) {
				again = append(again, d)
			}
		}
		if firstPass {
			retried = len(again)
			firstPass = false
		}
		if len(again) == len(pending) {
			// no progress, the rest is skipped for good
			log.Debug().Int("items", len(again)).
				Msg("Deltas left over after the last pass (parents not loaded).")
			break
		}
		pending = again
	}
	if retried > 0 {
		log.Debug().Int("items", retried).Int("total", len(items)).
			Msg("Deltas that needed a second pass (listed before their parent).")
	}
}

// Resync discards the delta link, so that the next poll (triggered right away)
// enumerates the whole drive and reports every item's current state. Items
// that were missed earlier are created, and (in loaded folders) items that no
// longer exist on the server are removed.
func (f *Filesystem) Resync() {
	f.ctl.resync.Store(true)
	f.beginCheck(true)
	f.wakeLoop()
}

func (f *Filesystem) resyncRequested() bool { return f.ctl.resync.Load() }
func (f *Filesystem) finishResync()         { f.ctl.resync.Store(false) }

// removeStale removes items that a complete enumeration of the drive did not
// report, after the server confirmed that they are gone. Only server-side
// items without any local changes or pending upload are touched.
func (f *Filesystem) removeStale(reported map[string]*graph.DriveItem) {
	var candidates []*Inode
	f.metadata.Range(func(key, value interface{}) bool {
		inode := value.(*Inode)
		id := key.(string)
		if _, ok := reported[id]; ok || isLocalID(id) || id == "" {
			return true
		}
		if inode.ParentID() == "" { // root
			return true
		}
		candidates = append(candidates, inode)
		return true
	})

	// children before their folders
	sort.Slice(candidates, func(i, j int) bool {
		return strings.Count(candidates[i].Path(), "/") > strings.Count(candidates[j].Path(), "/")
	})
	for _, inode := range candidates {
		id := inode.ID()
		path := inode.Path()
		inode.RLock()
		busy := inode.hasChanges || len(inode.children) > 0
		inode.RUnlock()
		if busy || f.uploads.IsUploadPending(id) || f.content.IsOpen(id) {
			continue
		}
		if f.GetID(id) != inode { // changed meanwhile
			continue
		}
		// Ask the server, the enumeration could have been taken before an
		// item of ours finished uploading.
		_, err := graph.GetItem(id, f.auth)
		if err == nil || !strings.HasPrefix(err.Error(), "HTTP 404 - ") {
			continue
		}
		log.Info().Str("id", id).Str("path", path).
			Msg("Removing local item that no longer exists on the server (resync).")
		f.recordActivity(path, status.ActionDeleted)
		f.changes.notePath(relPath(path))
		f.DeleteID(id)
	}
}

// fullDeltaLink starts a complete enumeration of the drive, used to recover when
// the server no longer accepts our delta token.
const fullDeltaLink = "/me/drive/root/delta"

type deltaResponse struct {
	NextLink  string             `json:"@odata.nextLink,omitempty"`
	DeltaLink string             `json:"@odata.deltaLink,omitempty"`
	Values    []*graph.DriveItem `json:"value,omitempty"`
}

// Polls the delta endpoint and return deltas + whether or not to continue
// polling. Does not perform deduplication. Note that changes from the local
// client will actually appear as deltas from the server (there is no
// distinction between local and remote changes from the server's perspective,
// everything is a delta, regardless of where it came from).
func (f *Filesystem) pollDeltas(auth *graph.Auth) ([]*graph.DriveItem, bool, error) {
	resp, err := graph.Get(f.deltaLink, auth)
	if err != nil {
		return make([]*graph.DriveItem, 0), false, err
	}

	// A body we cannot make sense of (a captive portal's HTML page after a
	// resume, a truncated response, an error object sent with status 200) must
	// not touch deltaLink: replacing it with "" would make every later poll
	// "succeed" without ever returning another change.
	page := deltaResponse{}
	if err := json.Unmarshal(resp, &page); err != nil {
		return make([]*graph.DriveItem, 0), false,
			fmt.Errorf("unreadable delta response: %w", err)
	}

	// If the server does not provide a `@odata.nextLink` item, it means we've
	// reached the end of this polling cycle and should not continue until the
	// next poll interval.
	if page.NextLink != "" {
		f.deltaLink = strings.TrimPrefix(page.NextLink, graph.GraphURL)
		return page.Values, true, nil
	}
	if page.DeltaLink == "" {
		// every valid last page carries the link for the next poll
		return make([]*graph.DriveItem, 0), false,
			errors.New("delta response had neither a nextLink nor a deltaLink")
	}
	f.deltaLink = strings.TrimPrefix(page.DeltaLink, graph.GraphURL)
	return page.Values, false, nil
}

// applyDelta diagnoses and applies a server-side change to our local state.
// Things we care about (present in the local cache):
// * Deleted items
// * Changed content remotely, but not locally
// * New items in a folder we have locally
func (f *Filesystem) applyDelta(delta *graph.DriveItem) error {
	// remember where the item was, to tell if it newly arrived somewhere
	oldParentID, existed := "", false
	if entry, ok := f.metadata.Load(delta.ID); ok {
		existed = true
		oldParentID = entry.(*Inode).ParentID()
	}

	err := f.applyDeltaItem(delta)

	// Keep pinned content current: re-check pinned files after any change
	// (the worker re-downloads them if their content changed), and walk
	// folders that newly appeared in or were moved into a pinned folder.
	if delta.Deleted == nil && f.pins != nil {
		if entry, ok := f.metadata.Load(delta.ID); ok {
			inode := entry.(*Inode)
			isNew := !existed || oldParentID != inode.ParentID()
			if (!inode.IsDir() || isNew) && f.isPinned(inode) {
				f.pins.enqueue(delta.ID)
			}
		}
	}
	return err
}

// applyDeltaItem does the actual work of applyDelta.
func (f *Filesystem) applyDeltaItem(delta *graph.DriveItem) error {
	id := delta.ID
	name := delta.Name
	if delta.Parent == nil {
		// e.g. the drive root in a full enumeration, nothing to apply
		return nil
	}
	parentID := delta.Parent.ID
	ctx := log.With().
		Str("id", id).
		Str("parentID", parentID).
		Str("name", name).
		Logger()
	ctx.Debug().Msg("Applying delta")

	// diagnose and act on what type of delta we're dealing with

	// do we have it at all?
	parent := f.GetID(parentID)
	if parent == nil {
		// Nothing needs to be applied, item not in cache, so latest copy will
		// be pulled down next time it's accessed. (Unless the parent is
		// another delta of this cycle, see applyDeltas.)
		ctx.Trace().
			Str("delta", "skip").
			Msg("Skipping delta, item's parent not in cache.")
		return errParentMissing
	}

	local := f.GetID(id)

	// was it deleted?
	if delta.Deleted != nil {
		if delta.IsDir() && local != nil && local.HasChildren() {
			// from docs: you should only delete a folder locally if it is empty
			// after syncing all the changes.
			ctx.Warn().Str("delta", "delete").
				Msg("Refusing delta deletion of non-empty folder as per API docs.")
			return errDirNotEmpty
		}
		ctx.Info().Str("delta", "delete").
			Msg("Applying server-side deletion of item.")
		if local != nil {
			f.recordActivity(local.Path(), status.ActionDeleted)
			f.changes.notePath(relPath(local.Path()))
		}
		f.DeleteID(id)
		return nil
	}

	// does the item exist locally? if not, add the delta to the cache under the
	// appropriate parent
	prevID := "" // local ID the item was tracked under before this delta, if any
	if local == nil {
		// Our own upload may have created this item, without the upload loop
		// having noticed yet. Match it by ID rather than by name, the item
		// may already have been renamed or moved on the server.
		if localID := f.uploads.UploadedAs(id); isLocalID(localID) && localID != "" {
			if local = f.GetID(localID); local != nil {
				ctx.Info().
					Str("localID", localID).
					Msg("Item was created by our own upload, moving it to its new ID.")
				prevID = localID
				if err := f.MoveID(localID, id); err != nil {
					ctx.Error().
						Str("localID", localID).
						Err(err).
						Msg("Could not move item to new, nonlocal ID!")
				}
			}
		}
	}
	if local == nil {
		// check if we don't have it here first
		local = f.cachedChild(parentID, name)
		if local != nil {
			localID := local.ID()
			ctx.Info().
				Str("localID", localID).
				Msg("Local item already exists under different ID.")
			if isLocalID(localID) {
				prevID = localID
				if err := f.MoveID(localID, id); err != nil {
					ctx.Error().
						Str("localID", localID).
						Err(err).
						Msg("Could not move item to new, nonlocal ID!")
				}
			}
		} else {
			// If we never listed the parent, inserting would make a partial
			// children list look complete and hide its other entries. The
			// first listing fetches everything, this item included.
			if !parent.childrenKnown() {
				ctx.Trace().Str("delta", "skip").
					Msg("Skipping delta, parent folder was not listed yet.")
				return nil
			}
			ctx.Info().Str("delta", "create").
				Msg("Creating inode from delta.")
			created := NewInodeDriveItem(delta)
			f.InsertChild(parentID, created)
			f.recordActivity(created.Path(), status.ActionAdded)
			f.stateChanged(id)
			return nil
		}
	}

	// was the item moved?
	localName := local.Name()
	if local.ParentID() != parentID || local.Name() != name {
		log.Info().
			Str("parent", local.ParentID()).
			Str("name", localName).
			Str("newParent", parentID).
			Str("newName", name).
			Str("id", id).
			Str("delta", "rename").
			Msg("Applying server-side rename")
		oldParentID := local.ParentID()
		// local rename only
		f.MovePath(oldParentID, parentID, localName, name, f.auth)
		// do not return, there may be additional changes
	}

	// Finally, check if the content/metadata of the remote has changed.
	// "Interesting" changes must be synced back to our local state without
	// data loss or corruption. Currently the only thing the local filesystem
	// actually modifies remotely is the actual file data, so we simply accept
	// the remote metadata changes that do not deal with the file's content
	// changing.
	local.RLock()
	localETag := local.DriveItem.ETag
	localModTime := local.DriveItem.ModTimeUnix()
	local.RUnlock()
	if delta.ETagIsMatch(localETag) {
		ctx.Trace().Str("delta", "skip").Msg("Skipping, no changes relative to local state.")
		return nil
	}

	if delta.IsDir() {
		// folders have no content to lose, just accept newer remote metadata
		if delta.ModTimeUnix() > localModTime {
			ctx.Info().Str("delta", "overwrite").
				Msg("Overwriting local folder metadata with server version.")
			local.Lock()
			local.DriveItem.ModTime = delta.ModTime
			local.DriveItem.Size = delta.Size
			local.DriveItem.ETag = delta.ETag
			local.Unlock()
			return nil
		}
		ctx.Trace().Str("delta", "skip").Msg("Skipping, no changes relative to local state.")
		return nil
	}

	// Files: content hashes, not modification times, decide what happens. The
	// local copy may have pending changes that must never be overwritten.
	ids := []string{id, prevID}
	remoteHash := ""
	if delta.File != nil {
		remoteHash = delta.File.Hashes.QuickXorHash
	}
	local.RLock()
	localHash := ""
	if local.DriveItem.File != nil {
		localHash = local.DriveItem.File.Hashes.QuickXorHash
	}
	localChanges := local.hasChanges
	local.RUnlock()
	localChanges = localChanges || f.uploads.IsUploadPending(ids...)

	action := conflict.Resolve(remoteHash, localHash, localChanges, f.uploads.UploadedHashes(ids...))
	if action == conflict.Overwrite && !f.overwriteFromDelta(local, delta, ids, ctx) {
		// local was modified while we were deciding, re-evaluate as changed
		action = conflict.Resolve(remoteHash, localHash, true, f.uploads.UploadedHashes(ids...))
	}

	switch action {
	case conflict.Overwrite:
		return nil
	case conflict.Skip:
		if remoteHash != "" && strings.EqualFold(remoteHash, localHash) {
			// the server confirmed it has the content we have
			local.Lock()
			local.serverHash = remoteHash
			local.Unlock()
		}
		return nil
	case conflict.Conflict:
		ctx.Warn().
			Str("delta", "conflict").
			Str("localHash", localHash).
			Str("remoteHash", remoteHash).
			Msg("Conflict detected: item changed on server while local changes " +
				"are not yet uploaded. Keeping local version, saving server " +
				"version as a conflict copy.")
		if err := f.createConflictCopy(delta, ctx); err != nil {
			ctx.Error().Err(err).Str("delta", "conflict").
				Msg("Could not create conflict copy of server version! The " +
					"local version will still be uploaded, the overwritten server " +
					"version can only be recovered from OneDrive's version history.")
			return err
		}
		// remember that we have seen (and preserved) this server version
		local.Lock()
		local.DriveItem.ETag = delta.ETag
		local.Unlock()
		return nil
	case conflict.Unknown:
		ctx.Warn().Str("delta", "conflict").
			Msg("Item changed on server but has no content hash to compare " +
				"against, keeping local changes and not overwriting them.")
		return nil
	}

	ctx.Trace().Str("delta", "skip").Msg("Skipping, no changes relative to local state.")
	return nil
}

// remoteHashOf returns the QuickXorHash of a server item, if any.
func remoteHashOf(item *graph.DriveItem) string {
	if item.File == nil {
		return ""
	}
	return item.File.Hashes.QuickXorHash
}

// overwriteFromDelta replaces a local file's metadata with the server's and
// invalidates its cached content so the new version gets downloaded on next
// open. Returns false (and changes nothing) if the local item has changes or
// a pending upload, which is re-checked here under the inode lock.
func (f *Filesystem) overwriteFromDelta(local *Inode, delta *graph.DriveItem, ids []string, ctx zerolog.Logger) bool {
	local.Lock()
	defer local.Unlock()
	// lock order: inode -> upload manager (the upload manager never takes
	// inode locks while holding its own)
	if local.hasChanges || f.uploads.IsUploadPending(ids...) {
		return false
	}
	ctx.Info().Str("delta", "overwrite").
		Msg("Overwriting local item, no local changes to preserve.")
	local.DriveItem.ModTime = delta.ModTime
	local.DriveItem.Size = delta.Size
	local.DriveItem.ETag = delta.ETag
	local.DriveItem.File = delta.File
	local.serverHash = remoteHashOf(delta)
	// Open() would also notice the hash mismatch and re-download, but drop the
	// stale content now. Files that are currently open keep their fd and get
	// refreshed by the checksum check on the next Open().
	if id := local.DriveItem.ID; !f.content.IsOpen(id) && f.content.HasContent(id) {
		ctx.Debug().Str("delta", "overwrite").Msg("Invalidating cached content.")
		f.content.Delete(id)
	}
	f.stateChanged(local.DriveItem.ID)
	return true
}

// createConflictCopy preserves the server version of a file as a new local
// item under a conflict name (which is then uploaded). The original item keeps
// its ID and local content, so its pending upload will replace the server
// version - upload sessions do not send an If-Match header and use
// conflictBehavior=replace, so this cannot fail on an eTag mismatch.
func (f *Filesystem) createConflictCopy(delta *graph.DriveItem, ctx zerolog.Logger) error {
	if delta.File == nil || delta.File.Hashes.QuickXorHash == "" {
		return errors.New("server item has no content hash")
	}
	parentID := delta.Parent.ID

	// pick a name that is not already taken in the parent folder
	host := conflict.Hostname()
	now := time.Now()
	var name string
	for n := 1; ; n++ {
		name = conflict.Name(delta.Name, host, now, n)
		if child, _ := f.GetChild(parentID, name, f.auth); child == nil {
			break
		}
		if n >= 100 {
			return errors.New("could not find a free conflict name")
		}
	}

	item := *delta
	item.ID = localID()
	item.Name = name
	item.ETag = ""
	item.Deleted = nil
	parent := *delta.Parent
	item.Parent = &parent
	file := *delta.File
	item.File = &file
	if item.ModTime == nil {
		item.ModTime = &now
	}
	copyInode := NewInodeDriveItem(&item)

	// download the server version into the cache under the new ID
	fd, err := f.content.Open(item.ID)
	if err != nil {
		return err
	}
	size, err := graph.GetItemContentStream(delta.ID, f.auth, fd)
	if err != nil {
		f.content.Delete(item.ID)
		return err
	}
	if !copyInode.VerifyChecksum(graph.QuickXORHashStream(fd)) {
		// most likely the server copy changed again since the delta (possibly
		// our own upload landed first)
		f.content.Delete(item.ID)
		return errors.New("downloaded server version did not match delta checksum")
	}
	f.content.Close(item.ID)
	copyInode.DriveItem.Size = size

	f.InsertChild(parentID, copyInode)
	ctx.Warn().
		Str("delta", "conflict").
		Str("conflictName", name).
		Str("conflictID", item.ID).
		Msg("Saved server version as conflict copy, uploading it.")
	f.noteConflict(copyInode.Path())
	f.stateChanged(item.ID)
	return f.uploads.QueueUpload(copyInode)
}
