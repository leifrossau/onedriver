// Package status holds the plain data types that describe the state of a
// mounted filesystem. It is shared by the fs package (which produces them) and
// the D-Bus service (which exports them), so neither has to import the other.
package status

// States reported in Status.State.
const (
	StateIdle    = "idle"
	StateSyncing = "syncing"
	StatePaused  = "paused"
	StateOffline = "offline"
	StateError   = "error"
)

// Activity actions, as reported by GetRecent.
const (
	ActionUploaded   = "uploaded"
	ActionDownloaded = "downloaded"
	ActionDeleted    = "deleted"
	ActionConflict   = "conflict"
	ActionError      = "error"
)

// Notification kinds, as sent in the Notify signal.
const (
	KindConflict     = "conflict"
	KindUploadFailed = "upload-failed"
	KindAuthExpired  = "auth-expired"
	KindError        = "error"
)

// Status is a snapshot of the sync state of a filesystem.
type Status struct {
	Account          string
	Paused           bool
	Offline          bool
	PendingUploads   uint32
	PendingDownloads uint32
	PinnedFiles      uint32
	PinnedBytes      uint64
	CacheBytes       uint64
	QuotaUsed        uint64
	QuotaTotal       uint64
	LastSync         int64 // unix time, 0 = never
	LastError        string
	// AuthExpired is true while there is no valid login, only Login fixes it
	AuthExpired bool
	// HasError is true while an upload was dropped or authentication failed
	HasError bool
}

// State derives the overall state. Precedence: error > paused > offline >
// syncing > idle.
func (s Status) State() string {
	return DeriveState(s.HasError, s.Paused, s.Offline, s.PendingUploads, s.PendingDownloads)
}

// DeriveState computes the overall state, see Status.State.
func DeriveState(hasError, paused, offline bool, pendingUploads, pendingDownloads uint32) string {
	switch {
	case hasError:
		return StateError
	case paused:
		return StatePaused
	case offline:
		return StateOffline
	case pendingUploads > 0 || pendingDownloads > 0:
		return StateSyncing
	}
	return StateIdle
}

// Activity is one entry of the recent activity list.
type Activity struct {
	Path   string // relative to the mountpoint
	Time   int64  // unix time
	Action string
}

// Notification is something the user should be told about.
type Notification struct {
	Kind    string
	Path    string
	Message string
}

// Transfer directions and states, as reported by GetTransfers.
const (
	DirUpload   = "upload"
	DirDownload = "download"

	TransferQueued   = "queued"
	TransferActive   = "active"
	TransferRetrying = "retrying"
)

// Transfer is an upload or download that is queued, running or waiting to be
// retried.
type Transfer struct {
	Path       string // relative to the mountpoint
	Direction  string
	State      string
	BytesDone  uint64
	BytesTotal uint64
	QueuedAt   int64 // unix time
}
