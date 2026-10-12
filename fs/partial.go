package fs

import (
	"errors"
	"os"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/status"
	"github.com/rs/zerolog"
)

// On-demand partial reads.
//
// Opening a file that is not cached (or whose cache is outdated) for reading
// does not download anything: the file becomes "partial" (see ranges.go) and
// Read fetches only the chunks it needs with HTTP Range requests, writing them
// into the sparse cache file. Sequential reads grow a read-ahead window that
// is fetched in the background. Once every byte has been fetched, the file is
// hashed once and, if the QuickXorHash matches, it is an ordinary fully cached
// (and verified) file from then on. On a mismatch the ranges are dropped and
// the file is downloaded in full.
//
// Files of at most partialSmallFile bytes are downloaded in full on their
// first Read. Before the first local change (Write, truncate) and when a file
// is opened for writing, partial files are downloaded in full first, so local
// changes (and thus uploads) always start from complete content. Pinned files
// are hydrated with full downloads as before.

const (
	// chunks are the unit of fetching and of deduplication
	partialChunk int64 = 256 << 10
	// largest read-ahead window for sequential reads
	partialMaxReadahead int64 = 8 << 20
	// largest single Range request
	partialMaxRequest int64 = 8 << 20
	// files up to this size are downloaded in full on their first read
	partialSmallFile int64 = 1 << 20
)

// errPartialFallback means the ranges could not be used and the file must be
// downloaded in full.
var errPartialFallback = errors.New("partial content unusable, full download needed")

// remoteHashOfInode returns the QuickXorHash the server reported for an inode
// along with whether it has local changes.
func remoteHashOfInode(inode *Inode) (hash string, hasChanges bool) {
	inode.RLock()
	defer inode.RUnlock()
	if inode.DriveItem.File != nil {
		hash = inode.DriveItem.File.Hashes.QuickXorHash
	}
	return hash, inode.hasChanges
}

// downloadInProgress returns the channel of a running full download of id
// (closed when it finishes), or nil.
func (f *Filesystem) downloadInProgress(id string) chan struct{} {
	f.downloadsM.Lock()
	defer f.downloadsM.Unlock()
	return f.downloads[id]
}

// openLazy is Open for reading a file that has no local changes: it uses the
// cached content if it is complete and current, and otherwise makes the file
// partial without downloading anything. ok=false means the caller should use
// the full download path (a full download is running right now).
func (f *Filesystem) openLazy(ctx zerolog.Logger, inode *Inode, fd *os.File, remoteHash string) (fuse.Status, bool) {
	id := inode.ID()
	for attempt := 0; attempt < 10; attempt++ {
		if p := f.partialGet(id); p != nil {
			p.mu.Lock()
			current := p.hash == strings.ToLower(remoteHash)
			p.mu.Unlock()
			if current {
				return fuse.OK, true
			}
		} else if status, done := f.useCachedContent(ctx, inode, fd, remoteHash); done {
			return status, true
		}

		unlock, ok := f.tryLockDownload(id)
		if ok {
			defer unlock()
			return f.resetPartial(ctx, inode, remoteHash), true
		}
		// somebody else is downloading or preparing the file (a concurrent
		// Open), wait for them and check again
		if ch := f.downloadInProgress(id); ch != nil {
			<-ch
		}
	}
	return fuse.OK, false
}

// resetPartial (re)starts partial caching of an inode's content for the
// version with remoteHash, dropping whatever was cached. The caller holds the
// download lock of the inode's ID.
func (f *Filesystem) resetPartial(ctx zerolog.Logger, inode *Inode, remoteHash string) fuse.Status {
	id := inode.ID()
	inode.Lock()
	defer inode.Unlock()
	if f.GetID(id) != inode {
		return fuse.EAGAIN
	}
	if inode.hasChanges {
		return fuse.OK // local content is newer, keep it
	}
	if inode.DriveItem.File == nil ||
		!strings.EqualFold(inode.DriveItem.File.Hashes.QuickXorHash, remoteHash) {
		return fuse.EAGAIN // changed again meanwhile
	}
	if _, err := f.content.Open(id); err != nil { // make sure the file exists
		ctx.Error().Err(err).Msg("Could not create cache file.")
		return fuse.EIO
	}
	f.verifiedForget(id)
	if _, err := f.partialStart(id, remoteHash, int64(inode.DriveItem.Size)); err != nil {
		ctx.Error().Err(err).Msg("Could not prepare cache file for partial content.")
		return fuse.EIO
	}
	ctx.Debug().Uint64("size", inode.DriveItem.Size).Msg("Content will be fetched on demand.")
	f.stateChanged(id)
	return fuse.OK
}

// ensureFull makes sure the content of a partially cached file is complete
// (downloading it in full if necessary). Files that are not partial are left
// alone.
func (f *Filesystem) ensureFull(ctx zerolog.Logger, inode *Inode) fuse.Status {
	id := inode.ID()
	for attempt := 0; attempt < 5; attempt++ {
		if !f.isPartial(id) {
			return fuse.OK
		}
		hash, _ := remoteHashOfInode(inode)
		if hash == "" {
			return fuse.EIO
		}
		unlock, wait := f.lockDownload(id)
		if wait {
			continue
		}
		if !f.isPartial(id) {
			unlock()
			return fuse.OK
		}
		ctx.Info().Msg("Downloading the rest of a partially cached file.")
		st := f.downloadContent(ctx, inode, nil, hash)
		unlock()
		if st == fuse.EAGAIN {
			if inode = f.GetID(id); inode == nil {
				return fuse.ENOENT
			}
			continue
		}
		return st
	}
	return fuse.EIO
}

// readPartial makes sure the bytes [off, off+size) of a partially cached file
// are in the cache file, fetching missing chunks. Returns fuse.OK once they
// are (or the file is not partial any more).
func (f *Filesystem) readPartial(ctx zerolog.Logger, inode *Inode, off, size int64) fuse.Status {
	id := inode.ID()
	for attempt := 0; attempt < 10; attempt++ {
		p := f.partialGet(id)
		if p == nil {
			return fuse.OK
		}
		hash, hasChanges := remoteHashOfInode(inode)
		if hasChanges {
			return fuse.OK
		}
		if ch := f.downloadInProgress(id); ch != nil {
			<-ch // a full download is running, it will fill everything
			continue
		}

		offline := f.IsOffline()
		p.mu.Lock()
		if p.dead {
			p.mu.Unlock()
			continue
		}
		if hash == "" || p.hash != strings.ToLower(hash) {
			// the remote file changed (with the file open): start over
			p.mu.Unlock()
			if hash == "" {
				return fuse.EIO
			}
			unlock, ok := f.tryLockDownload(id)
			if !ok {
				continue
			}
			ctx.Info().Msg("Remote content changed, dropping cached ranges.")
			st := f.resetPartial(ctx, inode, hash)
			unlock()
			if st != fuse.OK && st != fuse.EAGAIN {
				return st
			}
			if st == fuse.EAGAIN {
				if inode = f.GetID(id); inode == nil {
					return fuse.ENOENT
				}
			}
			continue
		}
		fileSize := p.size
		if fileSize <= partialSmallFile {
			p.mu.Unlock()
			if offline {
				return fuse.EIO
			}
			return f.ensureFull(ctx, inode)
		}
		end := off + size
		if end > fileSize {
			end = fileSize
		}
		if off >= end {
			p.mu.Unlock()
			return fuse.OK
		}

		// sequential access grows the read-ahead window
		if p.nextOff >= 0 && off >= p.nextOff-partialChunk && off <= p.nextOff+partialChunk {
			if p.readahead == 0 {
				p.readahead = partialChunk
			} else if p.readahead < partialMaxReadahead {
				p.readahead *= 2
			}
			if end > p.nextOff {
				p.nextOff = end
			}
		} else {
			p.readahead = 0
			p.nextOff = end
		}

		have := p.ranges.contains(off, end)
		if have && p.readahead == 0 {
			p.mu.Unlock()
			return fuse.OK
		}
		if offline {
			p.mu.Unlock()
			if have {
				return fuse.OK
			}
			return fuse.EIO
		}

		// claim the missing chunks needed now and those to read ahead
		var need, ahead []int64
		var waits []chan struct{}
		first := off / partialChunk
		lastNeed := (end - 1) / partialChunk
		last := lastNeed
		raEnd := end
		if p.readahead > 0 {
			raEnd = end + p.readahead
			if raEnd > fileSize {
				raEnd = fileSize
			}
			last = (raEnd - 1) / partialChunk
		}
		for c := first; c <= last; c++ {
			cs, ce := c*partialChunk, (c+1)*partialChunk
			if ce > fileSize {
				ce = fileSize
			}
			if p.ranges.contains(cs, ce) {
				continue
			}
			if ch, busy := p.inflight[c]; busy {
				if c <= lastNeed {
					waits = append(waits, ch)
				}
				continue
			}
			if c <= lastNeed {
				need = append(need, c)
			} else {
				ahead = append(ahead, c)
			}
		}
		// Read ahead in large requests: only once at least half of the
		// window is missing (or the window reaches the end of the file).
		if int64(len(ahead))*partialChunk < p.readahead/2 && raEnd < fileSize {
			ahead = nil
		}
		for _, c := range need {
			p.inflight[c] = make(chan struct{})
		}
		for _, c := range ahead {
			p.inflight[c] = make(chan struct{})
		}
		gen := p.gen
		p.mu.Unlock()

		if len(ahead) > 0 {
			if f.startLoop() {
				go func(inode *Inode, p *partialFile, gen uint64, hash string, size int64, chunks []int64) {
					defer f.loops.Done()
					if err := f.fetchChunks(ctx, inode, p, gen, hash, size, chunks); err != nil {
						ctx.Debug().Err(err).Msg("Read-ahead failed.")
					}
				}(inode, p, gen, hash, fileSize, ahead)
			} else {
				f.releaseChunks(p, ahead)
			}
		}
		err := f.fetchChunks(ctx, inode, p, gen, hash, fileSize, need)
		for _, ch := range waits {
			<-ch
		}
		if errors.Is(err, errPartialFallback) {
			return f.ensureFull(ctx, inode)
		}
		if err != nil {
			ctx.Error().Err(err).Msg("Could not fetch content range.")
			return fuse.EIO
		}
		p.mu.Lock()
		done := p.dead || p.ranges.contains(off, end)
		p.mu.Unlock()
		if done && f.partialGet(id) != p {
			continue // became complete (or was reset), check again
		}
		if done {
			return fuse.OK
		}
	}
	return fuse.EIO
}

// releaseChunks gives up the claims on chunks without fetching them.
func (f *Filesystem) releaseChunks(p *partialFile, chunks []int64) {
	p.mu.Lock()
	for _, c := range chunks {
		if ch, ok := p.inflight[c]; ok {
			delete(p.inflight, c)
			close(ch)
		}
	}
	p.mu.Unlock()
}

// fetchChunks downloads the claimed chunks (in runs of consecutive chunks of
// at most partialMaxRequest bytes), writes them into the cache file and
// records them, unless the partial state was reset or dropped meanwhile. All
// claims are released when it returns. When the file became complete, it is
// verified (see finishPartial).
func (f *Filesystem) fetchChunks(ctx zerolog.Logger, inode *Inode, p *partialFile, gen uint64, hash string, size int64, chunks []int64) error {
	if len(chunks) == 0 {
		return nil
	}
	id := inode.ID()
	f.transfers.queue(id, status.DirDownload, inode.Path(), uint64(size))
	f.transfers.update(id, status.DirDownload, nil, func(e *transferEntry) {
		e.state = status.TransferActive
	})
	defer func() {
		if f.downloadInProgress(id) == nil {
			f.transfers.remove(id, status.DirDownload)
		}
	}()

	perRequest := partialMaxRequest / partialChunk
	for i := 0; i < len(chunks); {
		j := i + 1
		for j < len(chunks) && chunks[j] == chunks[j-1]+1 && int64(j-i) < perRequest {
			j++
		}
		run := chunks[i:j]
		i = j

		start := run[0] * partialChunk
		end := (run[len(run)-1] + 1) * partialChunk
		if end > size {
			end = size
		}
		data, err := graph.GetItemContentRange(id, f.auth, start, end, size)
		if err != nil {
			f.releaseChunks(p, chunks[i-len(run):])
			if errors.Is(err, graph.ErrShortRange) {
				ctx.Warn().Err(err).Msg("Item is smaller than reported, downloading it in full.")
				return errPartialFallback
			}
			return err
		}

		complete, err := f.commitRange(id, p, gen, start, data)
		f.releaseChunks(p, run)
		if err != nil {
			f.releaseChunks(p, chunks[i:])
			return err
		}
		if complete {
			f.releaseChunks(p, chunks[i:])
			return f.finishPartial(ctx, inode, p, hash)
		}
	}
	return nil
}

// commitRange writes data at off into the cache file of id and records the
// range, unless p was reset (gen changed) or dropped. Returns whether the file
// is complete now.
func (f *Filesystem) commitRange(id string, p *partialFile, gen uint64, off int64, data []byte) (bool, error) {
	// a separate fd: the shared one may be closed by a Flush any time
	file, err := os.OpenFile(f.content.contentPath(id), os.O_WRONLY, 0600)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // deleted or evicted meanwhile
		}
		return false, err
	}
	defer file.Close()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead || p.gen != gen {
		return false, nil
	}
	if _, err := file.WriteAt(data, off); err != nil {
		return false, err
	}
	p.ranges = p.ranges.add(off, off+int64(len(data)))
	f.persistPartial(id, p)
	f.transfers.setDone(id, status.DirDownload, uint64(p.ranges.total()))
	return p.ranges.contains(0, p.size), nil
}

// finishPartial verifies a partial file whose ranges cover everything. On a
// hash match the file becomes an ordinary, verified cached file. On a
// mismatch the ranges are dropped and the file is downloaded in full.
func (f *Filesystem) finishPartial(ctx zerolog.Logger, inode *Inode, p *partialFile, hash string) error {
	id := inode.ID()
	unlock, ok := f.tryLockDownload(id)
	if !ok {
		return nil // somebody is downloading it in full already
	}
	defer unlock()

	p.mu.Lock()
	gen := p.gen
	ready := !p.dead && p.hash == strings.ToLower(hash) && p.ranges.contains(0, p.size)
	p.mu.Unlock()
	if !ready {
		return nil
	}

	file, err := os.Open(f.content.contentPath(id))
	if err != nil {
		return err
	}
	got := f.hashContent(file)
	st, err := file.Stat()
	file.Close()
	if err != nil {
		return err
	}

	inode.Lock()
	p.mu.Lock()
	if p.dead || p.gen != gen || f.GetID(id) != inode {
		p.mu.Unlock()
		inode.Unlock()
		return nil
	}
	if strings.EqualFold(got, hash) {
		p.dead = true
		p.gen++
		p.mu.Unlock()
		f.partials.CompareAndDelete(id, p)
		f.partialDropRecord(id)
		f.verifiedPut(id, newVerified(hash, st))
		inode.DriveItem.Size = uint64(st.Size())
		inode.Unlock()
		ctx.Info().Msg("All ranges fetched and verified, file is fully cached.")
		f.stateChanged(id)
		f.recordActivity(inode.Path(), status.ActionDownloaded)
		return nil
	}

	ctx.Warn().Str("expected", hash).Str("got", got).
		Msg("Content assembled from ranges does not match the item's hash, " +
			"dropping the ranges and downloading the file in full.")
	p.gen++
	p.ranges = nil
	truncateSparse(f.content.contentPath(id), p.size)
	f.persistPartial(id, p)
	p.mu.Unlock()
	inode.Unlock()

	if st := f.downloadContent(ctx, inode, nil, hash); st != fuse.OK {
		return errors.New("full download after failed verification failed")
	}
	return nil
}
