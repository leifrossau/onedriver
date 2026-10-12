package fs

// Pinning ("always keep on this device") and cache eviction ("free up space").
//
// Model: every inode carries an explicit pin state, which is "inherit" for
// almost all items. Whether an item is pinned ("effectively pinned") is
// decided by the nearest item, walking from the item itself up through its
// ancestors, that has an explicit state; if there is none it is not pinned.
// This makes pinned folders sticky without any bookkeeping: whatever appears
// inside them later (created locally, arriving via delta, moved in) inherits
// the pin, and whatever is moved out loses it, unless it was pinned
// explicitly itself. Pinning or unpinning a folder applies to its whole
// subtree, so explicit states below it are reset to "inherit". Unpinning an
// item inside a pinned folder stores an explicit "unpinned" override.
//
// A background worker (pinWorker) downloads the content of pinned files that
// is missing or outdated, walking pinned folders (and fetching their children
// from the server where they are not known yet), and periodically evicts
// unpinned, clean content if eviction is enabled.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

// pinState is the explicit pin state stored on an inode.
type pinState uint8

const (
	// pinInherit means the item is pinned if its parent folder is.
	pinInherit pinState = iota
	// pinPinned means the item (and, for folders, everything inside it that
	// does not say otherwise) is kept on this device.
	pinPinned
	// pinUnpinned means the item is not pinned even if its parent is.
	pinUnpinned
)

// CacheOptions configures automatic eviction of cached file content. The
// zero value disables eviction.
type CacheOptions struct {
	// CacheLimit is the maximum size of cached content in bytes. When it is
	// exceeded, the least recently used unpinned content is evicted. 0 means
	// no limit. Pinned content counts towards the limit but is never evicted.
	CacheLimit uint64
	// EvictAfter evicts unpinned content that was not opened for this long.
	// 0 disables age-based eviction.
	EvictAfter time.Duration
	// EvictInterval is how often eviction runs. Defaults to 10 minutes.
	EvictInterval time.Duration
}

func (o CacheOptions) enabled() bool {
	return o.CacheLimit > 0 || o.EvictAfter > 0
}

// PinStatus summarizes the hydration state of pinned items. Only items whose
// metadata is in memory are counted, which is the case for everything the
// worker has walked.
type PinStatus struct {
	// PinnedFiles is the number of (effectively) pinned files.
	PinnedFiles int
	// PinnedBytes is the total size of the pinned files.
	PinnedBytes uint64
	// PendingFiles is the number of pinned files whose content is not (yet
	// known to be) downloaded and current.
	PendingFiles int
	// PendingBytes is the total size of the pending files.
	PendingBytes uint64
	// Queued is the number of items waiting in the worker's queue (files and
	// folders still to be walked).
	Queued int
	// Active is the number of items being processed right now.
	Active int
}

const (
	pinConcurrency       = 3
	pinTick              = time.Second
	pinRescanInterval    = 30 * time.Minute
	pinBackoffBase       = 2 * time.Second
	pinMaxBackoff        = 5 * time.Minute
	pinMaxAttempts       = 8
	defaultEvictInterval = 10 * time.Minute
)

// errPinOffline is returned by worker jobs that cannot run while offline.
var errPinOffline = errors.New("offline")

type pinJob struct {
	id        string
	attempts  int
	notBefore time.Time
	queuedAt  time.Time
}

// pinWorker downloads pinned content in the background and evicts unpinned
// content. Its goroutine is started with the filesystem and stopped by
// Filesystem.Stop.
type pinWorker struct {
	f    *Filesystem
	wake chan struct{}

	// tunables, only changed by tests before start
	tick        time.Duration
	backoffBase time.Duration

	mu         sync.Mutex
	pending    map[string]*pinJob // queued items, by ID
	active     map[string]bool    // items being processed
	opts       CacheOptions
	lastEvict  time.Time
	lastRescan time.Time
	rescanNow  bool
	wasOffline bool
	// resolved is false until explicit pins stored in the database by a
	// previous session have been loaded
	resolved   bool
	stored     []string // stored pins still to be loaded
	storedRead bool
	// resolveOnline is true if stored pins must be looked up on the server
	// (online startup, where metadata is fetched fresh from the server and
	// the database copy may be outdated)
	resolveOnline bool
}

func newPinWorker(f *Filesystem) *pinWorker {
	return &pinWorker{
		f:             f,
		wake:          make(chan struct{}, 1),
		tick:          pinTick,
		backoffBase:   pinBackoffBase,
		pending:       make(map[string]*pinJob),
		active:        make(map[string]bool),
		rescanNow:     true,
		resolveOnline: !f.IsOffline(),
	}
}

func (w *pinWorker) start() {
	if !w.f.startLoop() {
		return
	}
	go func() {
		defer w.f.loops.Done()
		w.loop()
	}()
}

// signal wakes up the worker without blocking.
func (w *pinWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// enqueue schedules an item (file or folder) to be checked and hydrated.
func (w *pinWorker) enqueue(id string) {
	if id == "" {
		return
	}
	w.mu.Lock()
	if job, ok := w.pending[id]; ok {
		job.notBefore = time.Time{} // something changed, retry right away
	} else {
		w.pending[id] = &pinJob{id: id, queuedAt: time.Now()}
	}
	w.mu.Unlock()
	w.f.transfers.touch()
	w.signal()
}

// rescan makes the worker re-walk all pinned items on its next run.
func (w *pinWorker) rescan() {
	w.mu.Lock()
	w.rescanNow = true
	w.mu.Unlock()
	w.signal()
}

func (w *pinWorker) loop() {
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	var jobs sync.WaitGroup
	defer jobs.Wait()
	for {
		select {
		case <-w.f.stopCh:
			return
		case <-w.wake:
		case <-ticker.C:
		}
		w.step(&jobs)
	}
}

// step runs one round of the worker: rescans if due, starts queued jobs and
// runs eviction if due. Nothing happens while offline.
func (w *pinWorker) step(jobs *sync.WaitGroup) {
	if w.f.Paused() {
		return
	}
	if w.f.AuthExpired() {
		return
	}
	if w.f.IsOffline() {
		w.mu.Lock()
		w.wasOffline = true
		w.mu.Unlock()
		return
	}

	now := time.Now()
	w.mu.Lock()
	if w.wasOffline {
		// things may have changed while we could not check
		w.wasOffline = false
		w.rescanNow = true
	}
	doRescan := w.rescanNow || now.Sub(w.lastRescan) > pinRescanInterval
	if doRescan {
		w.rescanNow = false
		w.lastRescan = now
	}
	w.mu.Unlock()
	if doRescan {
		w.scanPinned()
	}

	w.mu.Lock()
	for id, job := range w.pending {
		if len(w.active) >= pinConcurrency {
			break
		}
		if w.active[id] || now.Before(job.notBefore) {
			continue
		}
		delete(w.pending, id)
		w.f.transfers.touch()
		w.active[id] = true
		jobs.Add(1)
		go func(job *pinJob) {
			defer jobs.Done()
			w.finish(job, w.process(job.id))
		}(job)
	}
	opts := w.opts
	interval := opts.EvictInterval
	if interval <= 0 {
		interval = defaultEvictInterval
	}
	doEvict := opts.enabled() && now.Sub(w.lastEvict) >= interval
	if doEvict {
		w.lastEvict = now
	}
	w.mu.Unlock()

	if doEvict {
		w.f.evictCache(opts)
	}
}

// finish records the result of a job, re-queueing it with backoff on errors.
func (w *pinWorker) finish(job *pinJob, err error) {
	w.mu.Lock()
	delete(w.active, job.id)
	if err != nil {
		if !errors.Is(err, errPinOffline) {
			job.attempts++
		}
		if job.attempts >= pinMaxAttempts {
			log.Error().Err(err).Str("id", job.id).Int("attempts", job.attempts).
				Msg("Giving up on downloading pinned item for now, will retry on the next rescan.")
		} else if _, queued := w.pending[job.id]; !queued {
			backoff := w.backoffBase << uint(job.attempts)
			if backoff > pinMaxBackoff || backoff <= 0 {
				backoff = pinMaxBackoff
			}
			job.notBefore = time.Now().Add(backoff)
			w.pending[job.id] = job
			w.f.transfers.touch()
			log.Warn().Err(err).Str("id", job.id).Int("attempts", job.attempts).
				Dur("backoff", backoff).Msg("Could not hydrate pinned item, will retry.")
		}
	}
	w.mu.Unlock()
	w.signal()
}

// process checks one pinned item: folders are walked, files downloaded if
// their cached content is missing or outdated.
func (w *pinWorker) process(id string) error {
	f := w.f
	inode := f.GetID(id)
	if inode == nil || !f.isPinned(inode) {
		return nil
	}
	if inode.IsDir() {
		return w.processDir(inode)
	}
	return f.hydrate(inode)
}

// processDir queues all pinned children of a pinned folder, fetching them from
// the server if they are not known yet.
func (w *pinWorker) processDir(inode *Inode) error {
	f := w.f
	children, err := f.GetChildrenID(inode.ID(), f.auth)
	if err != nil {
		return err
	}
	inode.RLock()
	known := inode.children != nil
	inode.RUnlock()
	if !known {
		// GetChildrenID pretends there are no children while offline
		return errPinOffline
	}
	for _, child := range children {
		if f.isPinned(child) {
			w.enqueue(child.ID())
		}
	}
	return nil
}

// scanPinned queues every explicitly pinned item, loading the ones stored by
// a previous session first.
func (w *pinWorker) scanPinned() {
	f := w.f
	w.mu.Lock()
	resolved, online := w.resolved, w.resolveOnline
	w.mu.Unlock()

	if !resolved {
		w.mu.Lock()
		if !w.storedRead {
			w.stored = f.storedPinnedIDs()
			w.storedRead = true
		}
		stored := w.stored
		w.mu.Unlock()
		remaining := f.loadStoredPins(stored, online)
		w.mu.Lock()
		w.stored = remaining
		if len(remaining) == 0 {
			w.resolved = true
		} else {
			// offline after all, try again on the next run
			w.rescanNow = true
		}
		w.mu.Unlock()
	}

	var ids []string
	f.metadata.Range(func(k, v interface{}) bool {
		inode := v.(*Inode)
		inode.RLock()
		pinned := inode.pin == pinPinned
		inode.RUnlock()
		if pinned {
			ids = append(ids, k.(string))
		}
		return true
	})
	for _, id := range ids {
		w.enqueue(id)
	}
}

// storedPinnedIDs returns the IDs of all items explicitly pinned in the
// database.
func (f *Filesystem) storedPinnedIDs() []string {
	var ids []string
	f.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMetadata)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if string(k) == "root" || !bytes.Contains(v, []byte(`"pin":`)) {
				return nil
			}
			var raw struct {
				Pin pinState `json:"pin"`
			}
			if json.Unmarshal(v, &raw) == nil && raw.Pin == pinPinned {
				ids = append(ids, string(k))
			}
			return nil
		})
	})
	return ids
}

// loadStoredPins brings explicitly pinned items recorded in the database by a
// previous session into memory. When online, they are looked up on the
// server and resolved by path from the root, so that their metadata (and
// that of their ancestors) is current; GetChildrenID carries over their pin
// state. Returns the IDs that could not be looked up because we are offline.
func (f *Filesystem) loadStoredPins(ids []string, online bool) []string {
	for i, id := range ids {
		if _, ok := f.metadata.Load(id); ok {
			continue
		}
		if !online {
			// offline startup: everything comes from the database anyway
			f.GetID(id)
			continue
		}
		item, err := graph.GetItem(id, f.auth)
		if err != nil {
			if graph.IsOffline(err) {
				return ids[i:]
			}
			log.Warn().Err(err).Str("id", id).
				Msg("Pinned item could not be found on the server, ignoring it.")
			continue
		}
		path := serverPath(item)
		if path == "" {
			continue
		}
		if inode, err := f.GetPath(path, f.auth); err != nil || inode.ID() != id {
			log.Warn().Err(err).Str("id", id).Str("path", path).
				Msg("Could not resolve pinned item by path, ignoring it.")
		}
	}
	return nil
}

// serverPath returns the path of a server item relative to the drive root,
// or "" if unknown.
func serverPath(item *graph.DriveItem) string {
	if item.Parent == nil {
		return ""
	}
	parent := item.Parent.Path
	idx := strings.Index(parent, "root:")
	if idx < 0 {
		return ""
	}
	parent = parent[idx+len("root:"):]
	return strings.TrimSuffix(parent, "/") + "/" + item.Name
}

// storedPins returns the explicit pin states of the given IDs, from memory if
// loaded, otherwise from the database. IDs with no explicit state are left
// out.
func (f *Filesystem) storedPins(ids []string) map[string]pinState {
	out := make(map[string]pinState)
	var missing []string
	for _, id := range ids {
		if entry, ok := f.metadata.Load(id); ok {
			inode := entry.(*Inode)
			inode.RLock()
			if inode.pin != pinInherit {
				out[id] = inode.pin
			}
			inode.RUnlock()
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out
	}
	f.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMetadata)
		if b == nil {
			return nil
		}
		for _, id := range missing {
			data := b.Get([]byte(id))
			if data == nil || !bytes.Contains(data, []byte(`"pin":`)) {
				continue
			}
			var raw struct {
				Pin pinState `json:"pin"`
			}
			if json.Unmarshal(data, &raw) == nil && raw.Pin != pinInherit {
				out[id] = raw.Pin
			}
		}
		return nil
	})
	return out
}

// isPinned returns whether an item is effectively pinned: the nearest
// explicit pin state of the item or its ancestors decides.
func (f *Filesystem) isPinned(inode *Inode) bool {
	cur := inode
	for depth := 0; cur != nil && depth < 512; depth++ {
		cur.RLock()
		state := cur.pin
		id := cur.DriveItem.ID
		parentID := ""
		if cur.DriveItem.Parent != nil {
			parentID = cur.DriveItem.Parent.ID
		}
		cur.RUnlock()
		switch state {
		case pinPinned:
			return true
		case pinUnpinned:
			return false
		}
		if parentID == "" || parentID == id {
			return false
		}
		cur = f.GetID(parentID)
	}
	return false
}

// IsPinned returns whether the item with the given ID is (effectively)
// pinned.
func (f *Filesystem) IsPinned(id string) bool {
	inode := f.GetID(id)
	return inode != nil && f.isPinned(inode)
}

// SetPinned pins or unpins an item. For folders, this applies to the whole
// subtree. It only records the new state and queues the item for the
// background worker, it does not download anything itself.
func (f *Filesystem) SetPinned(id string, pinned bool) error {
	inode := f.GetID(id)
	if inode == nil {
		return errors.New("item not found: " + id)
	}
	f.setPinned(inode, pinned)
	return nil
}

func (f *Filesystem) setPinned(inode *Inode, pinned bool) {
	inherited := false
	if parentID := inode.ParentID(); parentID != "" {
		if parent := f.GetID(parentID); parent != nil {
			inherited = f.isPinned(parent)
		}
	}
	state := pinInherit
	if pinned != inherited {
		state = pinUnpinned
		if pinned {
			state = pinPinned
		}
	}

	var changed []*Inode
	inode.Lock()
	if inode.pin != state {
		inode.pin = state
		changed = append(changed, inode)
	}
	inode.Unlock()
	if inode.IsDir() {
		changed = append(changed, f.resetDescendantPins(inode)...)
	}
	f.persistInodes(changed)

	id := inode.ID()
	f.stateChanged(id)
	if inode.IsDir() {
		f.noteDescendantsChanged(inode)
	}
	log.Info().Str("id", id).Str("path", inode.Path()).Bool("pinned", pinned).
		Msg("Pin state changed.")
	if pinned && f.pins != nil {
		f.pins.enqueue(id)
	}
}

// noteDescendantsChanged records every loaded item below a folder as changed
// (their effective pin state follows the folder). Memory only, bounded by the
// collector's cap.
func (f *Filesystem) noteDescendantsChanged(dir *Inode) {
	dir.RLock()
	queue := append([]string(nil), dir.children...)
	dir.RUnlock()
	seen := make(map[string]bool)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		f.stateChanged(id)
		if child := f.GetID(id); child != nil && child.IsDir() {
			child.RLock()
			queue = append(queue, child.children...)
			child.RUnlock()
		}
	}
}

// resetDescendantPins resets the explicit pin state of everything below a
// folder to "inherit" and returns the inodes that changed.
func (f *Filesystem) resetDescendantPins(dir *Inode) []*Inode {
	var changed []*Inode
	dir.RLock()
	queue := append([]string(nil), dir.children...)
	dir.RUnlock()
	seen := make(map[string]bool)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		child := f.GetID(id)
		if child == nil {
			continue
		}
		child.Lock()
		if child.pin != pinInherit {
			child.pin = pinInherit
			changed = append(changed, child)
		}
		queue = append(queue, child.children...)
		child.Unlock()
	}
	return changed
}

// persistInodes writes the metadata of the given inodes to disk right away
// (normally metadata is only written by the delta loop while online, but a
// pin must survive a restart even if it was set while offline).
func (f *Filesystem) persistInodes(inodes []*Inode) {
	if len(inodes) == 0 {
		return
	}
	data := make(map[string][]byte, len(inodes))
	for _, inode := range inodes {
		id := inode.ID()
		data[id] = inode.AsJSON()
		if id == f.root {
			data["root"] = data[id]
		}
	}
	err := f.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMetadata)
		for k, v := range data {
			if err := b.Put([]byte(k), v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Msg("Could not save pin state to disk.")
	}
}

// contentMatches returns true if the cached content for an ID exists and has
// the given hash. Files are only hashed if they changed since they were last
// verified. Does not use (or create) the shared cache fds.
func (f *Filesystem) contentMatches(id string, hash string) bool {
	if hash == "" {
		return false
	}
	st, err := f.content.Stat(id)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	state := newVerified(hash, st)
	if v, ok := f.verifiedGet(id); ok && v.same(state) {
		return true
	}
	file, err := os.Open(f.content.contentPath(id))
	if err != nil {
		return false
	}
	defer file.Close()
	if !strings.EqualFold(hash, f.hashContent(file)) {
		return false
	}
	// hashing may have raced with writes, so re-stat before trusting it
	if st2, err := f.content.Stat(id); err != nil || st2.Size() != st.Size() ||
		!st2.ModTime().Equal(st.ModTime()) {
		return false
	}
	f.verifiedPut(id, state)
	return true
}

// verifiedNow is a cheap (no hashing) check whether the cached content of id
// is known to have the given hash.
func (f *Filesystem) verifiedNow(id string, hash string) bool {
	v, ok := f.verifiedGet(id)
	if !ok || hash == "" || v.hash != strings.ToLower(hash) {
		return false
	}
	st, err := f.content.Stat(id)
	return err == nil && v.same(newVerified(hash, st))
}

// hydrate makes sure a file's content is cached and matches the server's
// version, downloading it if necessary. Files with local changes or pending
// uploads are left alone, their local content is newer than the server's.
func (f *Filesystem) hydrate(inode *Inode) error {
	id := inode.ID()
	if isLocalID(id) {
		return nil
	}
	ctx := log.With().Str("op", "hydrate").Str("id", id).Str("path", inode.Path()).Logger()
	for {
		inode.RLock()
		hasChanges := inode.hasChanges
		remoteHash := ""
		if inode.DriveItem.File != nil {
			remoteHash = inode.DriveItem.File.Hashes.QuickXorHash
		}
		inode.RUnlock()

		if hasChanges || f.uploads.IsUploadPending(id) {
			return nil
		}
		if remoteHash == "" {
			ctx.Warn().Msg("Pinned file has no content hash, cannot download it in the background.")
			return nil
		}
		if f.contentMatches(id, remoteHash) {
			return nil
		}
		if f.IsOffline() || f.AuthExpired() {
			return errPinOffline
		}

		unlock, wait := f.lockDownload(id)
		if wait {
			// Open() or another worker just downloaded it, check again
			continue
		}
		status := f.downloadContent(ctx, inode, nil, remoteHash)
		unlock()
		if status != fuse.OK {
			return fmt.Errorf("download failed: %v", status)
		}
		return nil
	}
}

// PinStatus returns counts describing the hydration progress of pinned
// items. It does no network I/O and no hashing.
func (f *Filesystem) PinStatus() PinStatus {
	var status PinStatus
	var roots []string
	f.metadata.Range(func(k, v interface{}) bool {
		inode := v.(*Inode)
		inode.RLock()
		pinned := inode.pin == pinPinned
		inode.RUnlock()
		if pinned {
			roots = append(roots, k.(string))
		}
		return true
	})

	seen := make(map[string]bool)
	queue := roots
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		entry, ok := f.metadata.Load(id)
		if !ok {
			continue
		}
		inode := entry.(*Inode)
		inode.RLock()
		state := inode.pin
		isDir := inode.DriveItem.IsDir() || inode.mode&fuse.S_IFDIR > 0
		children := append([]string(nil), inode.children...)
		size := inode.DriveItem.Size
		hasChanges := inode.hasChanges
		hash := ""
		if inode.DriveItem.File != nil {
			hash = inode.DriveItem.File.Hashes.QuickXorHash
		}
		inode.RUnlock()
		if state == pinUnpinned {
			continue
		}
		if isDir {
			queue = append(queue, children...)
			continue
		}
		status.PinnedFiles++
		status.PinnedBytes += size
		if !isLocalID(id) && !hasChanges && !f.verifiedNow(id, hash) {
			status.PendingFiles++
			status.PendingBytes += size
		}
	}

	if f.pins != nil {
		f.pins.mu.Lock()
		status.Queued = len(f.pins.pending)
		status.Active = len(f.pins.active)
		f.pins.mu.Unlock()
	}
	return status
}

// SetCacheOptions configures automatic eviction (disabled by default).
func (f *Filesystem) SetCacheOptions(opts CacheOptions) {
	if f.pins == nil {
		return
	}
	f.pins.mu.Lock()
	f.pins.opts = opts
	f.pins.lastEvict = time.Time{}
	f.pins.mu.Unlock()
	if opts.enabled() {
		log.Info().Uint64("cacheLimit", opts.CacheLimit).Dur("evictAfter", opts.EvictAfter).
			Msg("Automatic eviction of unpinned file content enabled.")
	}
	f.pins.signal()
}

// tryLockDownload is lockDownload, but fails instead of waiting if a download
// is in progress.
func (f *Filesystem) tryLockDownload(id string) (unlock func(), ok bool) {
	f.downloadsM.Lock()
	defer f.downloadsM.Unlock()
	if _, busy := f.downloads[id]; busy {
		return nil, false
	}
	ch := make(chan struct{})
	f.downloads[id] = ch
	return func() {
		f.downloadsM.Lock()
		delete(f.downloads, id)
		f.downloadsM.Unlock()
		close(ch)
	}, true
}

// Reasons content could not be evicted.
var (
	errEvictPinned  = errors.New("item is pinned")
	errEvictBusy    = errors.New("item is open or being downloaded")
	errEvictChanged = errors.New("item has local changes or a pending upload")
	errEvictUnknown = errors.New("cached content is not known to be on the server")
)

// evictFile removes the cached content of a file if that is safe: the file
// is not pinned, not open, has no local changes or pending upload, and its
// cached content is known to be identical to the server's. Returns whether
// content was removed (false with a nil error if there was nothing to evict).
func (f *Filesystem) evictFile(inode *Inode) (bool, error) {
	id := inode.ID()
	if inode.IsDir() {
		return false, nil
	}
	if isLocalID(id) {
		return false, errEvictChanged // never uploaded, this is the only copy
	}
	if _, err := f.content.Stat(id); err != nil {
		return false, nil // not cached
	}
	if f.isPinned(inode) {
		return false, errEvictPinned
	}
	nodeID := inode.NodeID()
	if f.hasOpenHandles(nodeID) || f.content.IsOpen(id) {
		return false, errEvictBusy
	}
	inode.RLock()
	hasChanges := inode.hasChanges
	hash := ""
	if inode.DriveItem.File != nil {
		hash = inode.DriveItem.File.Hashes.QuickXorHash
	}
	serverHash := inode.serverHash
	inode.RUnlock()
	if hasChanges || f.uploads.IsUploadPending(id) {
		return false, errEvictChanged
	}
	if hash == "" || !strings.EqualFold(hash, serverHash) || !f.contentMatches(id, hash) {
		return false, errEvictUnknown
	}

	unlock, ok := f.tryLockDownload(id)
	if !ok {
		return false, errEvictBusy
	}
	defer unlock()
	inode.Lock()
	defer inode.Unlock()
	// re-check everything that could have changed, under the inode lock (lock
	// order: inode -> upload manager)
	if inode.hasChanges || f.uploads.IsUploadPending(id) || f.GetID(id) != inode {
		return false, errEvictChanged
	}
	if f.hasOpenHandles(nodeID) {
		return false, errEvictBusy
	}
	if !f.verifiedNow(id, hash) {
		return false, errEvictUnknown
	}
	if !f.content.Evict(id) {
		return false, errEvictBusy
	}
	log.Info().Str("id", id).Str("name", inode.DriveItem.Name).Msg("Evicted cached content.")
	return true, nil
}

// EvictContent frees up the space used by the cached content of an item, or
// for folders, of all items below it (as far as their metadata is loaded).
// Pinned, open and modified items are skipped. For a single file, the reason
// it could not be evicted is returned as an error. Returns the number of
// files evicted.
func (f *Filesystem) EvictContent(id string) (int, error) {
	inode := f.GetID(id)
	if inode == nil {
		return 0, errors.New("item not found: " + id)
	}
	if !inode.IsDir() {
		ok, err := f.evictFile(inode)
		if ok {
			return 1, nil
		}
		return 0, err
	}

	evicted := 0
	seen := make(map[string]bool)
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		node := f.GetID(cur)
		if node == nil {
			continue
		}
		if node.IsDir() {
			node.RLock()
			queue = append(queue, node.children...)
			node.RUnlock()
			continue
		}
		if ok, _ := f.evictFile(node); ok {
			evicted++
		}
	}
	return evicted, nil
}

// evictCache runs automatic eviction: content not accessed for EvictAfter is
// evicted, then least recently used content until the cache is below
// CacheLimit. Only unpinned, clean, closed files are evicted.
func (f *Filesystem) evictCache(opts CacheOptions) {
	entries, err := os.ReadDir(f.content.directory)
	if err != nil {
		log.Error().Err(err).Msg("Could not list content cache for eviction.")
		return
	}
	type candidate struct {
		id    string
		size  uint64
		atime time.Time
	}
	var total uint64
	var candidates []candidate
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		size := uint64(info.Size())
		total += size
		name := entry.Name()
		if strings.HasPrefix(name, "temp-") || isLocalID(name) {
			continue
		}
		atime := info.ModTime()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
			if mtime := info.ModTime(); mtime.After(atime) {
				atime = mtime
			}
		}
		candidates = append(candidates, candidate{id: name, size: size, atime: atime})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].atime.Before(candidates[j].atime)
	})

	now := time.Now()
	evicted, freed := 0, uint64(0)
	for _, c := range candidates {
		overLimit := opts.CacheLimit > 0 && total > opts.CacheLimit
		tooOld := opts.EvictAfter > 0 && now.Sub(c.atime) > opts.EvictAfter
		if !overLimit && !tooOld {
			// sorted by access time, nothing after this qualifies either
			break
		}
		// only consider items whose metadata is in memory: only those can
		// be known to be on the server (see Inode.serverHash)
		entry, ok := f.metadata.Load(c.id)
		if !ok {
			continue
		}
		if ok, _ := f.evictFile(entry.(*Inode)); ok {
			evicted++
			freed += c.size
			total -= c.size
		}
	}
	if evicted > 0 {
		log.Info().Int("files", evicted).Uint64("bytes", freed).Uint64("cacheSize", total).
			Msg("Evicted unpinned content from the cache.")
	}
	if opts.CacheLimit > 0 && total > opts.CacheLimit {
		log.Warn().Uint64("cacheSize", total).Uint64("cacheLimit", opts.CacheLimit).
			Msg("Content cache is over its limit, but nothing more can be evicted " +
				"(pinned, open or modified files).")
	}
}
