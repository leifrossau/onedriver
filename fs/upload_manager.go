package fs

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

const maxUploadsInFlight = 5

// number of content hashes remembered per item for conflict detection
const maxUploadHashes = 10

var bucketUploads = []byte("uploads")

// UploadManager is used to manage and retry uploads.
type UploadManager struct {
	queue         chan *UploadSession
	deletionQueue chan string
	moveQueue     chan moveRequest
	sessions      map[string]*UploadSession
	auth          *graph.Auth
	fs            *Filesystem
	db            *bolt.DB

	// sessionsM guards writes to sessions (which only happen on the
	// uploadLoop goroutine) against reads from other goroutines (the delta
	// loop), as well as reserved and hashes. It is never held while acquiring
	// an inode lock.
	sessionsM sync.RWMutex
	reserved  map[string]int      // uploads queued but not yet received by uploadLoop
	hashes    map[string][]string // recent content hashes we have uploaded, by ID
	aliases   map[string]idAlias  // old ID -> new ID of items whose ID changed
}

// How long an old -> new ID mapping is remembered after an item's ID changed,
// to redirect sessions, reservations and cancellations still using the old ID.
const aliasTTL = 10 * time.Minute

type idAlias struct {
	newID string
	at    time.Time
}

type moveRequest struct {
	oldID, newID string
	done         chan struct{}
}

// NewUploadManager creates a new queue/thread for uploads
func NewUploadManager(duration time.Duration, db *bolt.DB, fs *Filesystem, auth *graph.Auth) *UploadManager {
	manager := UploadManager{
		queue:         make(chan *UploadSession),
		deletionQueue: make(chan string, 1000), // FIXME - why does this chan need to be buffered now???
		sessions:      make(map[string]*UploadSession),
		reserved:      make(map[string]int),
		hashes:        make(map[string][]string),
		aliases:       make(map[string]idAlias),
		moveQueue:     make(chan moveRequest),
		auth:          auth,
		db:            db,
		fs:            fs,
	}
	db.View(func(tx *bolt.Tx) error {
		// Add any incomplete sessions from disk - any sessions here were never
		// finished. The most likely cause of this is that the user shut off
		// their computer or closed the program after starting the upload.
		b := tx.Bucket(bucketUploads)
		if b == nil {
			// bucket does not exist yet, bail out early
			return nil
		}
		return b.ForEach(func(key []byte, val []byte) error {
			session := &UploadSession{}
			err := json.Unmarshal(val, session)
			if err != nil {
				log.Error().Err(err).Msg("Failure restoring upload sessions from disk.")
				return err
			}
			session.cancel(auth) // uploads are currently non-resumable
			manager.sessions[session.ID] = session
			manager.recordHashLocked(session.ID, session.QuickXORHash)
			return nil
		})
	})
	if fs != nil {
		if fs.startLoop() {
			go func() {
				defer fs.loops.Done()
				manager.uploadLoop(duration)
			}()
		}
	} else {
		go manager.uploadLoop(duration)
	}
	return &manager
}

// stopped returns a channel that is closed once the filesystem is stopped.
func (u *UploadManager) stopped() <-chan struct{} {
	if u.fs == nil {
		return nil
	}
	return u.fs.stopCh
}

// uploadLoop manages the deduplication and tracking of uploads
func (u *UploadManager) uploadLoop(duration time.Duration) {
	ticker := time.NewTicker(duration)
	defer ticker.Stop()
	stop := u.stopped()
	for {
		select {
		case <-stop:
			return

		case session := <-u.queue: // new sessions
			u.resolveSession(session)
			// deduplicate sessions for the same item
			if old, exists := u.sessions[session.ID]; exists {
				old.cancel(u.auth)
			}
			contents, _ := json.Marshal(session)
			u.db.Batch(func(tx *bolt.Tx) error {
				// persist to disk in case the user shuts off their computer or
				// kills onedriver prematurely
				b, _ := tx.CreateBucketIfNotExists(bucketUploads)
				return b.Put([]byte(session.ID), contents)
			})
			// swap the reservation for the real session atomically so the
			// delta loop never sees this upload as "not pending"
			u.sessionsM.Lock()
			u.sessions[session.ID] = session
			u.releaseLocked(session.reservedID)
			u.recordHashLocked(session.ID, session.QuickXORHash)
			u.sessionsM.Unlock()

		case cancelID := <-u.deletionQueue: // remove uploads for deleted items
			u.finishUpload(u.resolveID(cancelID))

		case req := <-u.moveQueue: // an item's ID changed (local -> server ID)
			u.moveUpload(req.oldID, req.newID)
			close(req.done)

		case <-ticker.C: // periodically start uploads, or remove them if done/failed
			// The number of uploads in flight is derived from session states
			// on every tick instead of being tracked with a counter, so it
			// cannot drift when sessions are retried, replaced or dropped.
			inFlight := 0
			for _, session := range u.sessions {
				if session.getState() == uploadStarted {
					inFlight++
				}
			}
			for _, session := range u.sessions {
				switch session.getState() {
				case uploadNotStarted:
					// max active upload sessions are capped at this limit for faster
					// uploads of individual files and also to prevent possible server-
					// side throttling that can cause errors.
					if inFlight < maxUploadsInFlight && !u.fs.Paused() {
						inFlight++
						// mark as started synchronously so the next tick
						// counts it even if the goroutine has not run yet
						session.setState(uploadStarted, nil)
						go session.Upload(u.auth)
					}

				case uploadErrored:
					session.retries++
					if session.retries > 5 {
						log.Error().
							Str("id", session.ID).
							Str("name", session.Name).
							Err(session).
							Int("retries", session.retries).
							Msg("Upload session failed too many times and was dropped. " +
								"Local changes were NOT uploaded to OneDrive; the file is kept " +
								"marked as modified and will be uploaded the next time it is saved.")
						u.finishUpload(session.ID)
						u.fs.noteUploadDropped(session.ID, u.sessionPath(session), session)
						// Do not leave the inode looking clean: hasChanges was
						// cleared when the upload was queued, and with the
						// session gone IsUploadPending is false too, so delta
						// would overwrite the unuploaded local content with the
						// server's copy. Re-mark it as modified instead.
						inode := u.fs.GetID(session.OldID)
						if inode == nil {
							inode = u.fs.GetID(session.ID)
						}
						if inode != nil {
							inode.Lock()
							inode.hasChanges = true
							inode.Unlock()
						}
						continue
					}

					log.Warn().
						Str("id", session.ID).
						Str("name", session.Name).
						Err(session).
						Msg("Upload session failed, will retry from beginning.")
					session.cancel(u.auth) // cancel large sessions
					session.setState(uploadNotStarted, nil)

				case uploadComplete:
					log.Info().
						Str("id", session.ID).
						Str("oldID", session.OldID).
						Str("name", session.Name).
						Msg("Upload completed!")

					// ID changed during upload, move to new ID
					if session.OldID != session.ID {
						u.recordHash(session.ID, session.QuickXORHash)
						// MoveID would deadlock on the upload loop, re-key
						// directly (this sets session.OldID to the new ID)
						oldID := session.OldID
						u.moveUpload(oldID, session.ID)
						err := u.fs.moveID(oldID, session.ID)
						if err != nil {
							log.Error().
								Str("id", session.ID).
								Str("oldID", session.OldID).
								Str("name", session.Name).
								Err(err).
								Msg("Could not move inode to new ID!")
						}
					}

					// inode will exist at the new ID now, but we check if inode
					// is nil to see if the item has been deleted since upload start
					if inode := u.fs.GetID(session.ID); inode != nil {
						inode.Lock()
						inode.DriveItem.ETag = session.ETag
						// the server now has this content, so a clean
						// cached copy of it may be evicted
						inode.serverHash = session.QuickXORHash
						inode.Unlock()
					}

					// the old ID is the one that was used to add it to the queue.
					// cleanup the session.
					u.finishUpload(session.OldID)
					u.fs.noteUploadDone(session.ID, session.OldID)
					u.fs.recordActivity(u.sessionPath(session), status.ActionUploaded)
				}
			}
		}
	}
}

// QueueUpload queues an item for upload.
func (u *UploadManager) QueueUpload(inode *Inode) error {
	id := inode.ID()
	u.reserveUpload(id)
	return u.queueReservedUpload(inode, id)
}

// reserveUpload marks an upload for an ID as pending before the session is
// actually created. Callers that clear an inode's hasChanges flag must reserve
// first, so that hasChanges and IsUploadPending never both report false while
// local content is still waiting to be uploaded. Must be paired with
// queueReservedUpload.
func (u *UploadManager) reserveUpload(id string) {
	u.sessionsM.Lock()
	u.reserved[u.resolveLocked(id)]++
	u.sessionsM.Unlock()
}

// queueReservedUpload queues an upload previously reserved under reservedID.
func (u *UploadManager) queueReservedUpload(inode *Inode, reservedID string) error {
	data := u.fs.getInodeContent(inode)
	session, err := NewUploadSession(inode, data)
	if err != nil {
		u.sessionsM.Lock()
		u.releaseLocked(reservedID)
		u.sessionsM.Unlock()
		return err
	}
	session.reservedID = reservedID
	select {
	case u.queue <- session:
		return nil
	case <-u.stopped():
		u.sessionsM.Lock()
		u.releaseLocked(reservedID)
		u.sessionsM.Unlock()
		return errors.New("filesystem is shutting down")
	}
}

// releaseLocked drops a reservation. sessionsM must be held.
func (u *UploadManager) releaseLocked(id string) {
	id = u.resolveLocked(id)
	if u.reserved[id] <= 1 {
		delete(u.reserved, id)
	} else {
		u.reserved[id]--
	}
}

// recordHash remembers that we uploaded content with this hash for an ID.
func (u *UploadManager) recordHash(id string, hash string) {
	u.sessionsM.Lock()
	u.recordHashLocked(id, hash)
	u.sessionsM.Unlock()
}

// recordHashLocked is recordHash, but sessionsM must be held.
func (u *UploadManager) recordHashLocked(id string, hash string) {
	if hash == "" {
		return
	}
	id = u.resolveLocked(id)
	hashes := u.hashes[id]
	for _, h := range hashes {
		if h == hash {
			return
		}
	}
	hashes = append(hashes, hash)
	if len(hashes) > maxUploadHashes {
		hashes = hashes[len(hashes)-maxUploadHashes:]
	}
	u.hashes[id] = hashes
}

// IsUploadPending returns true if an upload for any of the given IDs is
// queued, in progress, or waiting to be retried. Safe to call from any
// goroutine.
func (u *UploadManager) IsUploadPending(ids ...string) bool {
	u.sessionsM.RLock()
	defer u.sessionsM.RUnlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		id = u.resolveLocked(id)
		if _, exists := u.sessions[id]; exists || u.reserved[id] > 0 {
			return true
		}
	}
	return false
}

// UploadedHashes returns the content hashes this client has recently uploaded
// for any of the given IDs. Safe to call from any goroutine.
func (u *UploadManager) UploadedHashes(ids ...string) []string {
	u.sessionsM.RLock()
	defer u.sessionsM.RUnlock()
	out := make([]string, 0)
	for _, id := range ids {
		out = append(out, u.hashes[u.resolveLocked(id)]...)
	}
	return out
}

// CancelUpload is used to kill any pending uploads for a session
func (u *UploadManager) CancelUpload(id string) {
	select {
	case u.deletionQueue <- id:
	case <-u.stopped():
	}
}

// MoveUpload re-keys any pending upload (session, reservations and uploaded
// hashes) for an item whose ID changed from oldID to newID, typically a local
// ID being replaced by the server ID. The upload is kept, not cancelled, and
// from then on targets the item by its new ID. Uploads, reservations and
// cancellations that still use oldID later on are redirected to newID. Blocks
// until done; must not be called from the upload loop or with an inode lock
// held.
func (u *UploadManager) MoveUpload(oldID, newID string) {
	req := moveRequest{oldID: oldID, newID: newID, done: make(chan struct{})}
	select {
	case u.moveQueue <- req:
	case <-u.stopped():
		return
	}
	select {
	case <-req.done:
	case <-u.stopped():
	}
}

// moveUpload implements MoveUpload. Only called on the upload loop goroutine,
// which is the only writer of u.sessions.
func (u *UploadManager) moveUpload(oldID, newID string) {
	if oldID == newID || oldID == "" || newID == "" {
		return
	}
	now := time.Now()
	u.sessionsM.Lock()
	for id, alias := range u.aliases {
		if now.Sub(alias.at) > aliasTTL {
			delete(u.aliases, id)
		}
	}
	if u.resolveLocked(newID) == oldID {
		// would form a cycle, nothing sensible to do
		u.sessionsM.Unlock()
		return
	}
	u.aliases[oldID] = idAlias{newID: newID, at: now}

	if n := u.reserved[oldID]; n > 0 {
		delete(u.reserved, oldID)
		u.reserved[u.resolveLocked(newID)] += n
	}
	if hashes, exists := u.hashes[oldID]; exists {
		delete(u.hashes, oldID)
		for _, hash := range hashes {
			u.recordHashLocked(newID, hash)
		}
	}

	session, exists := u.sessions[oldID]
	keep := false
	if exists {
		delete(u.sessions, oldID)
		if _, taken := u.sessions[newID]; !taken {
			// a session already under the new ID was queued later, so it
			// is newer and wins, otherwise move this one over
			keep = true
			session.Lock()
			session.ID = newID
			session.OldID = newID
			session.Unlock()
			u.sessions[newID] = session
		}
	}
	u.sessionsM.Unlock()

	if exists {
		var contents []byte
		if keep {
			contents, _ = json.Marshal(session)
		}
		u.db.Batch(func(tx *bolt.Tx) error {
			b, _ := tx.CreateBucketIfNotExists(bucketUploads)
			b.Delete([]byte(oldID))
			if keep {
				return b.Put([]byte(newID), contents)
			}
			return nil
		})
	}
}

// UploadedAs returns the (local) ID that an upload was queued under, if that
// upload has already created the item with the given server ID on the server
// but the upload loop has not processed its completion yet (so the item is
// still tracked under its old ID). Returns "" if there is none. Safe to call
// from any goroutine.
func (u *UploadManager) UploadedAs(id string) string {
	u.sessionsM.RLock()
	defer u.sessionsM.RUnlock()
	for key, session := range u.sessions {
		session.Lock()
		sessionID := session.ID
		session.Unlock()
		if sessionID == id && key != id {
			return key
		}
	}
	return ""
}

// resolveLocked follows old -> new ID mappings. sessionsM must be held.
func (u *UploadManager) resolveLocked(id string) string {
	for i := 0; i < 8; i++ {
		alias, exists := u.aliases[id]
		if !exists {
			break
		}
		id = alias.newID
	}
	return id
}

// resolveID is resolveLocked for callers not holding sessionsM.
func (u *UploadManager) resolveID(id string) string {
	u.sessionsM.RLock()
	defer u.sessionsM.RUnlock()
	return u.resolveLocked(id)
}

// resolveSession points a newly queued session that was created under an ID
// which has since changed at the item's current ID. Only called on the upload
// loop goroutine, before the session is started.
func (u *UploadManager) resolveSession(session *UploadSession) {
	if id := u.resolveID(session.ID); id != session.ID {
		session.Lock()
		session.ID = id
		session.OldID = id
		session.Unlock()
	}
}

// finishUpload is an internal method that gets called when a session is
// completed. It cancels the session if one was in progress, and then deletes
// it from both memory and disk.
func (u *UploadManager) finishUpload(id string) {
	if session, exists := u.sessions[id]; exists {
		session.cancel(u.auth)
	}
	u.db.Batch(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketUploads); b != nil {
			b.Delete([]byte(id))
		}
		return nil
	})
	u.sessionsM.Lock()
	delete(u.sessions, id)
	u.sessionsM.Unlock()
}

// sessionState returns the state of the upload session for the given ID, and
// whether an upload is pending (a session exists or one has been reserved by
// Fsync). Safe to call from any goroutine, never blocks on the upload loop.
func (u *UploadManager) sessionState(id string) (state int, exists bool) {
	u.sessionsM.RLock()
	id = u.resolveLocked(id)
	session, exists := u.sessions[id]
	reserved := u.reserved[id] > 0
	u.sessionsM.RUnlock()
	if !exists {
		return uploadNotStarted, reserved
	}
	return session.getState(), true
}

// sessionPath returns the path of the item an upload session belongs to, or
// just its name if the item is gone. No locks may be held by the caller.
func (u *UploadManager) sessionPath(session *UploadSession) string {
	for _, id := range []string{session.ID, session.OldID} {
		if inode := u.fs.GetID(id); inode != nil {
			return inode.Path()
		}
	}
	return session.Name
}
