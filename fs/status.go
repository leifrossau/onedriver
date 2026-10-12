package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog/log"
)

// This file holds the plain Go side of the status/control interface that is
// exported over D-Bus (see docs/dbus-interface.md and fs/dbus). Nothing in
// here knows about D-Bus.

// ErrNotFound is returned by the path based status functions if the path does
// not exist.
var ErrNotFound = errors.New("no such file or directory")

const (
	maxRecent       = 100
	cacheSizeMaxAge = 10 * time.Second
	quotaMaxAge     = 2 * time.Minute
)

// syncControl is the state behind pausing, activity and error reporting.
type syncControl struct {
	paused atomic.Bool
	// loginRunning is set while the login window is open
	loginRunning atomic.Bool
	// wake interrupts the delta loop's sleep (Resume, SyncNow)
	wake chan struct{}
	// removes the auth failure subscription, set once by NewFilesystem
	cancelAuthHook func()

	mu            sync.Mutex
	lastSync      time.Time
	lastError     string
	authFailed    bool
	failedUploads map[string]string // item ID -> path of uploads that were dropped
	recent        []status.Activity // oldest first, at most maxRecent
	observers     map[int]func(status.Notification)
	nextObserver  int

	// expensive values, refreshed in the background
	cacheBytes   uint64
	cacheAt      time.Time
	cacheRunning bool
	quotaUsed    uint64
	quotaTotal   uint64
	quotaAt      time.Time
	quotaRunning bool
}

func newSyncControl() *syncControl {
	return &syncControl{
		wake:          make(chan struct{}, 1),
		failedUploads: make(map[string]string),
		observers:     make(map[int]func(status.Notification)),
	}
}

// relPath turns an inode path ("/dir/file") into a path relative to the
// mountpoint.
func relPath(p string) string {
	return strings.TrimPrefix(p, "/")
}

// recordActivity adds an entry to the ring buffer of recent activity.
func (f *Filesystem) recordActivity(path, action string) {
	c := f.ctl
	c.mu.Lock()
	c.recent = append(c.recent, status.Activity{
		Path: relPath(path), Time: time.Now().Unix(), Action: action,
	})
	if len(c.recent) > maxRecent {
		c.recent = append([]status.Activity(nil), c.recent[len(c.recent)-maxRecent:]...)
	}
	c.mu.Unlock()
}

// notify tells the subscribers about something the user should know. The
// subscribers are called synchronously and must not block.
func (f *Filesystem) notify(n status.Notification) {
	c := f.ctl
	c.mu.Lock()
	if n.Kind != status.KindConflict {
		c.lastError = n.Message
	}
	observers := make([]func(status.Notification), 0, len(c.observers))
	for _, o := range c.observers {
		observers = append(observers, o)
	}
	c.mu.Unlock()
	for _, o := range observers {
		o(n)
	}
}

// Subscribe registers a function that is called for every notification
// (conflict copy created, upload dropped, authentication failure). It is
// called synchronously from filesystem internals and must not block. The
// returned function removes the subscription.
func (f *Filesystem) Subscribe(handler func(status.Notification)) (cancel func()) {
	c := f.ctl
	c.mu.Lock()
	id := c.nextObserver
	c.nextObserver++
	c.observers[id] = handler
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.observers, id)
		c.mu.Unlock()
	}
}

// noteConflict is called when a conflict copy was created.
func (f *Filesystem) noteConflict(path string) {
	f.recordActivity(path, status.ActionConflict)
	f.notify(status.Notification{
		Kind: status.KindConflict, Path: relPath(path),
		Message: "Edited on two devices, the other version was saved as " + filepath.Base(path),
	})
}

// noteUploadDropped is called when an upload was given up after too many retries.
func (f *Filesystem) noteUploadDropped(id, path string, cause error) {
	f.stateChanged(id)
	msg := "Could not upload " + relPath(path)
	if cause != nil {
		msg += ": " + cause.Error()
	}
	f.ctl.mu.Lock()
	f.ctl.failedUploads[id] = relPath(path)
	f.ctl.mu.Unlock()
	f.recordActivity(path, status.ActionError)
	f.notify(status.Notification{Kind: status.KindUploadFailed, Path: relPath(path), Message: msg})
}

// noteUploadDone is called when an upload finished, which clears the error
// state of that item.
func (f *Filesystem) noteUploadDone(ids ...string) {
	for _, id := range ids {
		f.stateChanged(id)
	}
	f.ctl.mu.Lock()
	for _, id := range ids {
		delete(f.ctl.failedUploads, id)
	}
	f.ctl.mu.Unlock()
}

// noteAuthFailure is called when the access tokens could not be renewed.
func (f *Filesystem) noteAuthFailure(msg string) {
	f.ctl.mu.Lock()
	f.ctl.authFailed = true
	f.ctl.mu.Unlock()
	f.notify(status.Notification{Kind: status.KindAuthExpired, Message: msg})
}

// clearAuthFailure undoes noteAuthFailure after a successful login.
func (f *Filesystem) clearAuthFailure() {
	f.ctl.mu.Lock()
	f.ctl.authFailed = false
	f.ctl.lastError = ""
	f.ctl.mu.Unlock()
}

// AuthExpired returns whether there is no valid login. The filesystem then
// keeps serving cached files and keeps changes locally, but does not talk to
// the server until Login succeeds. Unlike offline mode, new files may still be
// written: they are uploaded after the login.
func (f *Filesystem) AuthExpired() bool {
	return f.auth != nil && f.auth.Expired()
}

// Login opens the interactive login window in a goroutine (at most one at a
// time) and returns right away; it returns an error if no window can be
// opened. On success the new tokens are used from then on, the error state is
// cleared and a sync is triggered; on failure the filesystem stays in the
// auth-expired state and the user is notified again.
func (f *Filesystem) Login() error {
	if err := graph.LoginUnavailable(); err != nil {
		return err
	}
	if !f.ctl.loginRunning.CompareAndSwap(false, true) {
		return nil // a login window is open already
	}
	go func() {
		defer f.ctl.loginRunning.Store(false)
		err := f.auth.Login()
		if err != nil {
			log.Warn().Err(err).Msg("Login did not succeed, staying logged out.")
			if f.auth.Expired() {
				f.noteAuthFailure("Login failed (" + err.Error() + "), click Log in to try again")
			}
			return
		}
		f.clearAuthFailure()
		f.SyncNow()
		if f.pins != nil {
			f.pins.signal()
		}
	}()
	return nil
}

// noteSyncSuccess is called after every successful delta poll.
func (f *Filesystem) noteSyncSuccess() {
	f.ctl.mu.Lock()
	f.ctl.lastSync = time.Now()
	f.ctl.authFailed = false
	f.ctl.mu.Unlock()
}

// Paused returns whether syncing is paused.
func (f *Filesystem) Paused() bool {
	return f.ctl.paused.Load()
}

// Pause stops starting new uploads, delta polling and background hydration.
// Local writes keep working and are uploaded after Resume.
func (f *Filesystem) Pause() {
	f.ctl.paused.Store(true)
}

// Resume undoes Pause and triggers a delta poll right away.
func (f *Filesystem) Resume() {
	f.ctl.paused.Store(false)
	f.SyncNow()
	if f.pins != nil {
		f.pins.signal()
	}
}

// SyncNow makes the delta loop poll the server right away.
func (f *Filesystem) SyncNow() {
	select {
	case f.ctl.wake <- struct{}{}:
	default:
	}
}

// sleepOrWake is sleep(), but returns early (still true) when SyncNow was
// called.
func (f *Filesystem) sleepOrWake(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-f.stopCh:
		return false
	case <-f.ctl.wake:
		return true
	case <-timer.C:
		return true
	}
}

// Recent returns up to limit of the most recent activity entries, newest
// first.
func (f *Filesystem) Recent(limit int) []status.Activity {
	f.ctl.mu.Lock()
	defer f.ctl.mu.Unlock()
	n := len(f.ctl.recent)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]status.Activity, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, f.ctl.recent[i])
	}
	return out
}

// Status returns a snapshot of the sync state. It never blocks on the network:
// the quota and the size of the content cache are refreshed in the background
// and may be slightly stale (or 0 right after startup).
func (f *Filesystem) Status() status.Status {
	pins := f.PinStatus()
	s := status.Status{
		Paused:           f.Paused(),
		Offline:          f.IsOffline(),
		PendingUploads:   f.uploads.pendingCount(),
		PendingDownloads: uint32(pins.PendingFiles),
		PinnedFiles:      uint32(pins.PinnedFiles),
		PinnedBytes:      pins.PinnedBytes,
	}
	if f.auth != nil {
		s.Account = f.auth.Account
	}

	c := f.ctl
	c.mu.Lock()
	if !c.lastSync.IsZero() {
		s.LastSync = c.lastSync.Unix()
	}
	s.LastError = c.lastError
	s.AuthExpired = f.auth != nil && f.auth.Expired()
	if s.AuthExpired && s.LastError == "" {
		s.LastError = graph.AuthExpiredMessage
	}
	s.HasError = c.authFailed || s.AuthExpired || len(c.failedUploads) > 0
	s.CacheBytes = c.cacheBytes
	s.QuotaUsed, s.QuotaTotal = c.quotaUsed, c.quotaTotal
	refreshCache := !c.cacheRunning && time.Since(c.cacheAt) > cacheSizeMaxAge
	if refreshCache {
		c.cacheRunning = true
	}
	refreshQuota := !c.quotaRunning && time.Since(c.quotaAt) > quotaMaxAge && !s.Offline && !s.Paused
	if refreshQuota {
		c.quotaRunning = true
	}
	c.mu.Unlock()

	if refreshCache {
		go f.refreshCacheSize()
	}
	if refreshQuota {
		go f.refreshQuota()
	}
	return s
}

func (f *Filesystem) refreshCacheSize() {
	var total uint64
	entries, _ := os.ReadDir(f.content.directory)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
	}
	f.ctl.mu.Lock()
	f.ctl.cacheBytes, f.ctl.cacheAt, f.ctl.cacheRunning = total, time.Now(), false
	f.ctl.mu.Unlock()
}

func (f *Filesystem) refreshQuota() {
	drive, err := graph.GetDrive(f.auth)
	f.ctl.mu.Lock()
	defer f.ctl.mu.Unlock()
	f.ctl.quotaRunning = false
	f.ctl.quotaAt = time.Now()
	if err != nil {
		log.Debug().Err(err).Msg("Could not fetch the drive quota.")
		return
	}
	f.ctl.quotaUsed, f.ctl.quotaTotal = drive.Quota.Used, drive.Quota.Total
}

// pendingCount is the number of uploads queued, in progress or retrying.
func (u *UploadManager) pendingCount() uint32 {
	u.sessionsM.RLock()
	defer u.sessionsM.RUnlock()
	n := len(u.sessions)
	for _, r := range u.reserved {
		n += r
	}
	return uint32(n)
}

// lookupPath finds an item by its path relative to the mountpoint. No inode
// lock is held while it may talk to the server.
func (f *Filesystem) lookupPath(path string) (*Inode, error) {
	path = "/" + strings.Trim(path, "/")
	inode, err := f.GetPath(path, f.auth)
	if inode == nil {
		// GetPath reports a missing item with this message, other errors
		// (network, server) are passed on
		if err != nil && !strings.Contains(err.Error(), "does not exist") &&
			!strings.Contains(err.Error(), "HTTP 404") {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return inode, nil
}

// FileState returns the sync state (as in the user.onedriver.state xattr) and
// whether the item at path (relative to the mountpoint) is pinned.
func (f *Filesystem) FileState(path string) (state string, pinned bool, err error) {
	inode, err := f.lookupPath(path)
	if err != nil {
		return "", false, err
	}
	return string(f.syncState(inode)), f.isPinned(inode), nil
}

// SetPinnedPath pins or unpins the item at path (relative to the mountpoint).
func (f *Filesystem) SetPinnedPath(path string, pinned bool) error {
	inode, err := f.lookupPath(path)
	if err != nil {
		return err
	}
	f.setPinned(inode, pinned)
	return nil
}

// EvictPath frees up the cached content of the item at path (relative to the
// mountpoint).
func (f *Filesystem) EvictPath(path string) error {
	inode, err := f.lookupPath(path)
	if err != nil {
		return err
	}
	if _, err := f.EvictContent(inode.ID()); err != nil {
		return err
	}
	return nil
}

// WebURL returns the OneDrive web address of the item at path (relative to
// the mountpoint), fetching it from the server if it is not cached.
func (f *Filesystem) WebURL(path string) (string, error) {
	inode, err := f.lookupPath(path)
	if err != nil {
		return "", err
	}
	inode.RLock()
	url, id := inode.DriveItem.WebURL, inode.DriveItem.ID
	inode.RUnlock()
	if url != "" {
		return url, nil
	}
	if isLocalID(id) {
		return "", fmt.Errorf("%s has not been uploaded yet", path)
	}
	item, err := graph.GetItem(id, f.auth)
	if err != nil {
		return "", err
	}
	if item.WebURL == "" {
		return "", errors.New("the server did not return a web URL")
	}
	return item.WebURL, nil
}

// IsNotFound reports whether err means that a path does not exist.
func (f *Filesystem) IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
