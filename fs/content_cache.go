package fs

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// LoopbackCache stores the content for files under a folder as regular files
type LoopbackCache struct {
	directory string
	fds       sync.Map
	// openM makes Open and Evict atomic with respect to each other, so content
	// is never evicted from under a file descriptor that is just being opened.
	openM sync.Mutex
	// onRemove, if set, is called after content was deleted, evicted or moved
	// away from an ID.
	onRemove func(id string)
}

func NewLoopbackCache(directory string) *LoopbackCache {
	os.Mkdir(directory, 0700)
	return &LoopbackCache{
		directory: directory,
		fds:       sync.Map{},
	}
}

// contentPath returns the path for the given content file
func (l *LoopbackCache) contentPath(id string) string {
	return filepath.Join(l.directory, id)
}

// Get reads a file's content from disk.
func (l *LoopbackCache) Get(id string) []byte {
	content, _ := os.ReadFile(l.contentPath(id))
	return content
}

// InsertContent writes file content to disk in a single bulk insert.
func (l *LoopbackCache) Insert(id string, content []byte) error {
	return os.WriteFile(l.contentPath(id), content, 0600)
}

// InsertStream inserts a stream of data
func (l *LoopbackCache) InsertStream(id string, reader io.Reader) (int64, error) {
	fd, err := l.Open(id)
	if err != nil {
		return 0, err
	}
	return io.Copy(fd, reader)
}

// Delete closes the fd AND deletes content from disk.
func (l *LoopbackCache) Delete(id string) error {
	l.Close(id)
	err := os.Remove(l.contentPath(id))
	l.removed(id)
	return err
}

// Move moves content from one ID to another
func (l *LoopbackCache) Move(oldID string, newID string) error {
	err := os.Rename(l.contentPath(oldID), l.contentPath(newID))
	l.removed(oldID)
	return err
}

// IsOpen returns true if the file is already opened somewhere
func (l *LoopbackCache) IsOpen(id string) bool {
	_, ok := l.fds.Load(id)
	return ok
}

// HasContent is used to find if we have a file or not in cache (in any state)
func (l *LoopbackCache) HasContent(id string) bool {
	// is it already open?
	_, ok := l.fds.Load(id)
	if ok {
		return ok
	}
	// is it on disk?
	_, err := os.Stat(l.contentPath(id))
	return err == nil
}

// Open returns a filehandle for subsequent access
func (l *LoopbackCache) Open(id string) (*os.File, error) {
	l.openM.Lock()
	defer l.openM.Unlock()
	if fd, ok := l.fds.Load(id); ok {
		// already opened, return existing fd
		return fd.(*os.File), nil
	}

	fd, err := os.OpenFile(l.contentPath(id), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	// Since we explicitly want to store *os.Files, we need to prevent the Go
	// GC from trying to be "helpful" and closing files for us behind the
	// scenes.
	// https://github.com/hanwen/go-fuse/issues/371#issuecomment-694799535
	runtime.SetFinalizer(fd, nil)
	l.fds.Store(id, fd)
	return fd, nil
}

// Close closes the currently open fd
func (l *LoopbackCache) Close(id string) {
	if fd, ok := l.fds.Load(id); ok {
		file := fd.(*os.File)
		file.Sync()
		file.Close()
		l.fds.Delete(id)
	}
}

// Evict removes the content file for an ID unless it currently has an open
// file descriptor, and returns whether it did. Unlike Delete, it never closes
// an fd in use.
func (l *LoopbackCache) Evict(id string) bool {
	l.openM.Lock()
	defer l.openM.Unlock()
	if _, ok := l.fds.Load(id); ok {
		return false
	}
	ok := os.Remove(l.contentPath(id)) == nil
	if ok {
		l.removed(id)
	}
	return ok
}

func (l *LoopbackCache) removed(id string) {
	if l.onRemove != nil {
		l.onRemove(id)
	}
}

// Stat returns the file info of the content file for an ID, without opening
// it.
func (l *LoopbackCache) Stat(id string) (os.FileInfo, error) {
	return os.Stat(l.contentPath(id))
}

// Install atomically replaces the content for an ID with the file at
// srcPath, unless the ID currently has an open fd (in which case false is
// returned and nothing is changed).
func (l *LoopbackCache) Install(id string, srcPath string) (bool, error) {
	l.openM.Lock()
	defer l.openM.Unlock()
	if _, ok := l.fds.Load(id); ok {
		return false, nil
	}
	return true, os.Rename(srcPath, l.contentPath(id))
}
