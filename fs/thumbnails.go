package fs

// Server-side thumbnails for file managers.
//
// File managers such as Dolphin (KIO PreviewJob) generate thumbnails by
// opening every image/document in a folder, which would make onedriver
// download all of them. KIO (like most freedesktop file managers) first looks
// for a ready-made thumbnail in the shared thumbnail cache,
// $XDG_CACHE_HOME/thumbnails/{normal,large,x-large}/<md5 of file URI>.png, and
// only opens the file if there is none. So whenever a directory is listed,
// a background worker downloads OneDrive's own thumbnails of the files that
// are not stored locally and writes them into that cache.
//
// Thumbnail Managing Standard:
//   https://specifications.freedesktop.org/thumbnail-spec/latest/
// KIO's lookup (thumbnailcache_p.cpp): the URI is QUrl::toEncoded(), the file
// name the md5 of it, Thumb::URI must equal it, Thumb::MTime must equal the
// file's mtime in seconds and Thumb::Size, if present, its size.

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	_ "image/gif" // decoders for server thumbnails
	_ "image/jpeg"
	"image/png"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/rs/zerolog/log"
)

// ThumbnailOptions configures the server-side thumbnail worker.
type ThumbnailOptions struct {
	// Enabled turns the worker on.
	Enabled bool
	// Mountpoint is the absolute path the filesystem is mounted on. It is
	// needed to compute the file:// URIs the thumbnails are filed under.
	Mountpoint string
	// CacheHome is the XDG cache directory containing "thumbnails". Defaults
	// to $XDG_CACHE_HOME or ~/.cache.
	CacheHome string
	// Workers is the number of concurrent downloads (default 2).
	Workers int
	// MinInterval is the minimum time between two requests (default 500ms,
	// that is at most 120 requests per minute).
	MinInterval time.Duration
}

// thumbTier is one directory of the thumbnail cache.
type thumbTier struct {
	dir  string
	size int // longest edge in pixels
}

var thumbTiers = []thumbTier{{"normal", 128}, {"large", 256}, {"x-large", 512}}

// thumbExts are the file types OneDrive renders thumbnails for (images,
// videos, PDFs and Office documents).
var thumbExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".bmp": true,
	".tif": true, ".tiff": true, ".webp": true, ".heic": true, ".heif": true,
	".avif": true, ".svg": true,
	".mp4": true, ".mov": true, ".m4v": true, ".avi": true, ".wmv": true, ".mkv": true,
	".pdf": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true, ".rtf": true, ".vsdx": true,
}

type thumbJob struct {
	key   string // id|mtime|size
	id    string
	path  string // path inside the mount, starting with /
	mtime uint64
	size  uint64
}

type thumbnailer struct {
	f    *Filesystem
	opts ThumbnailOptions
	root string // <cache home>/thumbnails
	jobs chan thumbJob

	mu     sync.Mutex
	queued map[string]bool // jobs waiting or running
	failed map[string]bool // no thumbnail available, do not ask again

	rateMu   sync.Mutex
	lastReq  time.Time
	requests int // server requests made, for tests
}

// SetThumbnails starts (or, with Enabled false, does not start) the worker
// that fills the freedesktop thumbnail cache with server-side thumbnails.
func (f *Filesystem) SetThumbnails(opts ThumbnailOptions) {
	if !opts.Enabled || opts.Mountpoint == "" {
		return
	}
	if opts.CacheHome == "" {
		opts.CacheHome, _ = os.UserCacheDir()
	}
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.MinInterval <= 0 {
		opts.MinInterval = 500 * time.Millisecond
	}
	if abs, err := filepath.Abs(opts.Mountpoint); err == nil {
		opts.Mountpoint = abs
	}
	t := &thumbnailer{
		f:      f,
		opts:   opts,
		root:   filepath.Join(opts.CacheHome, "thumbnails"),
		jobs:   make(chan thumbJob, 4096),
		queued: make(map[string]bool),
		failed: make(map[string]bool),
	}
	f.thumbsM.Lock()
	f.thumbs = t
	f.thumbsM.Unlock()
	for i := 0; i < opts.Workers; i++ {
		if !f.startLoop() {
			return
		}
		go func() {
			defer f.loops.Done()
			t.worker()
		}()
	}
}

// enqueueThumbnails is called for every directory listing.
func (f *Filesystem) enqueueThumbnails(children map[string]*Inode) {
	f.thumbsM.RLock()
	t := f.thumbs
	f.thumbsM.RUnlock()
	if t == nil {
		return
	}
	for _, c := range children {
		t.consider(c)
	}
}

func (t *thumbnailer) consider(c *Inode) {
	if c.IsDir() || c.Size() == 0 || !thumbExts[strings.ToLower(path.Ext(c.Name()))] {
		return
	}
	id := c.ID()
	// A file that is stored locally can be thumbnailed by the file manager
	// without a download. Files with local changes are not on the server yet.
	if t.f.content.HasContent(id) || c.HasChanges() || strings.HasPrefix(id, "local-") {
		return
	}
	job := thumbJob{id: id, path: c.Path(), mtime: c.ModTime(), size: c.Size()}
	job.key = id + "|" + strconv.FormatUint(job.mtime, 10) + "|" + strconv.FormatUint(job.size, 10)

	t.mu.Lock()
	skip := t.queued[job.key] || t.failed[job.key]
	if !skip {
		t.queued[job.key] = true
	}
	t.mu.Unlock()
	if skip {
		return
	}
	select {
	case t.jobs <- job:
	default: // queue full, the next listing retries
		t.mu.Lock()
		delete(t.queued, job.key)
		t.mu.Unlock()
	}
}

func (t *thumbnailer) worker() {
	for {
		select {
		case <-t.f.stopCh:
			return
		case job := <-t.jobs:
			t.process(job)
			t.mu.Lock()
			delete(t.queued, job.key)
			t.mu.Unlock()
		}
	}
}

// throttle spaces out requests; returns false when the filesystem stopped.
func (t *thumbnailer) throttle() bool {
	t.rateMu.Lock()
	defer t.rateMu.Unlock()
	if wait := t.opts.MinInterval - time.Since(t.lastReq); wait > 0 {
		if !t.f.sleep(wait) {
			return false
		}
	}
	t.lastReq = time.Now()
	t.requests++
	return true
}

func (t *thumbnailer) process(job thumbJob) {
	if t.f.IsOffline() || t.f.AuthExpired() || t.f.Paused() {
		return // listing the folder again retries
	}
	uri := thumbnailURI(t.opts.Mountpoint, job.path)
	paths := make([]string, len(thumbTiers))
	missing := false
	for i, tier := range thumbTiers {
		paths[i] = filepath.Join(t.root, tier.dir, thumbnailName(uri))
		if !thumbnailValid(paths[i], uri, job.mtime, job.size) {
			missing = true
		}
	}
	if !missing || t.f.content.HasContent(job.id) {
		return
	}
	if !t.throttle() {
		return
	}
	data, err := graph.GetItemThumbnail(job.id, graph.ThumbnailSize, t.f.auth)
	if err != nil {
		if errors.Is(err, graph.ErrNoThumbnail) {
			t.markFailed(job)
		} else {
			log.Debug().Err(err).Str("path", job.path).Msg("Could not fetch server thumbnail.")
		}
		return
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		log.Debug().Err(err).Str("path", job.path).Msg("Could not decode server thumbnail.")
		t.markFailed(job)
		return
	}
	text := []textChunk{
		{"Thumb::URI", uri},
		{"Thumb::MTime", strconv.FormatUint(job.mtime, 10)},
		{"Thumb::Size", strconv.FormatUint(job.size, 10)},
		{"Software", "onedriver"},
	}
	for i, tier := range thumbTiers {
		if thumbnailValid(paths[i], uri, job.mtime, job.size) {
			continue
		}
		out, err := encodePNGWithText(scaleToFit(src, tier.size), text)
		if err == nil {
			err = writeFileAtomic(paths[i], out)
		}
		if err != nil {
			log.Warn().Err(err).Str("path", paths[i]).Msg("Could not write thumbnail.")
			return
		}
	}
	log.Debug().Str("path", job.path).Msg("Cached server thumbnail.")
}

func (t *thumbnailer) markFailed(job thumbJob) {
	t.mu.Lock()
	t.failed[job.key] = true
	t.mu.Unlock()
}

// thumbnailURI computes the URI KIO files a thumbnail under, the way
// QUrl::fromLocalFile(...).toEncoded() spells it: everything except
// unreserved characters and the sub-delimiters, ":", "@" and "/" is
// percent-encoded (non-ASCII as UTF-8 bytes).
func thumbnailURI(mountpoint, p string) string {
	full := path.Join(filepath.ToSlash(mountpoint), p)
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(full); i++ {
		c := full[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("-._~!$&'()*+,;=:@/", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// thumbnailName is the file name of a URI's thumbnail: the lowercase hex md5
// of the URI plus ".png".
func thumbnailName(uri string) string {
	sum := md5.Sum([]byte(uri))
	return hex.EncodeToString(sum[:]) + ".png"
}

type textChunk struct{ key, value string }

// encodePNGWithText encodes img as PNG with tEXt chunks (which image/png
// cannot write) inserted right after IHDR.
func encodePNGWithText(img image.Image, text []textChunk) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	enc := buf.Bytes()
	const ihdrEnd = 8 + 4 + 4 + 13 + 4 // signature + length, type, data, crc
	if len(enc) < ihdrEnd {
		return nil, errors.New("short PNG")
	}
	var out bytes.Buffer
	out.Write(enc[:ihdrEnd])
	for _, c := range text {
		data := append(append([]byte(c.key), 0), c.value...)
		var head [4]byte
		binary.BigEndian.PutUint32(head[:], uint32(len(data)))
		out.Write(head[:])
		crc := crc32.NewIEEE()
		for _, part := range [][]byte{[]byte("tEXt"), data} {
			out.Write(part)
			crc.Write(part)
		}
		binary.BigEndian.PutUint32(head[:], crc.Sum32())
		out.Write(head[:])
	}
	out.Write(enc[ihdrEnd:])
	return out.Bytes(), nil
}

// readPNGText returns the tEXt chunks of a PNG file.
func readPNGText(data []byte) (map[string]string, error) {
	const sig = "\x89PNG\r\n\x1a\n"
	if !strings.HasPrefix(string(data), sig) {
		return nil, errors.New("not a PNG")
	}
	text := make(map[string]string)
	for pos := len(sig); pos+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		if n < 0 || pos+12+n > len(data) {
			return nil, errors.New("truncated PNG")
		}
		body := data[pos+8 : pos+8+n]
		if typ == "tEXt" {
			if i := bytes.IndexByte(body, 0); i > 0 {
				text[string(body[:i])] = string(body[i+1:])
			}
		}
		if typ == "IDAT" || typ == "IEND" {
			break
		}
		pos += 12 + n
	}
	return text, nil
}

// thumbnailValid reports whether the file at p is a thumbnail of the given
// URI that is current for the mtime and size.
func thumbnailValid(p, uri string, mtime, size uint64) bool {
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	text, err := readPNGText(data)
	if err != nil || text["Thumb::URI"] != uri || text["Thumb::MTime"] != strconv.FormatUint(mtime, 10) {
		return false
	}
	s, ok := text["Thumb::Size"]
	return !ok || s == strconv.FormatUint(size, 10)
}

// writeFileAtomic writes data to p (mode 0600, directories 0700) via a
// temporary file and a rename, so file managers never see partial files.
func writeFileAtomic(p string, data []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".onedriver-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// scaleToFit shrinks src (box filter) so its longest edge is at most max. It
// never enlarges.
func scaleToFit(src image.Image, max int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= max && sh <= max {
		return src
	}
	dw, dh := max, max
	if sw > sh {
		dh = (sh*max + sw/2) / sw
	} else {
		dw = (sw*max + sh/2) / sh
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA64(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			dst.SetRGBA64(x, y, color.RGBA64{uint16(r / n), uint16(g / n), uint16(bl / n), uint16(a / n)})
		}
	}
	return dst
}
