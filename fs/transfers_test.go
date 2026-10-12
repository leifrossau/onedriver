package fs

import (
	"bytes"
	"testing"

	"github.com/jstaf/onedriver/fs/status"
	"github.com/stretchr/testify/assert"
)

func TestTransferRegistry(t *testing.T) {
	t.Parallel()
	var r transferRegistry
	v0 := r.Version()

	r.queue("a", status.DirUpload, "a.txt", 100)
	r.queue("a", status.DirDownload, "a.txt", 50)
	assert.NotEqual(t, v0, r.Version())
	assert.Len(t, r.snapshot(), 2, "direction is part of the key")

	// updates of unknown entries are ignored
	r.setDone("nope", status.DirUpload, 5)
	r.setState("nope", status.DirUpload, status.TransferActive)
	assert.Len(t, r.snapshot(), 2)

	r.setState("a", status.DirUpload, status.TransferActive)
	r.setDone("a", status.DirUpload, 40)
	var up transferItem
	for _, it := range r.snapshot() {
		if it.dir == status.DirUpload {
			up = it
		}
	}
	assert.Equal(t, status.TransferActive, up.state)
	assert.Equal(t, uint64(40), up.done)
	assert.Equal(t, uint64(100), up.total)
	assert.Equal(t, "a.txt", up.name)

	// a retry resets the progress
	r.setState("a", status.DirUpload, status.TransferRetrying)
	for _, it := range r.snapshot() {
		if it.dir == status.DirUpload {
			assert.Zero(t, it.done)
		}
	}

	// re-keying follows the ID change
	r.rekey("a", "b", status.DirUpload)
	ids := map[string]bool{}
	for _, it := range r.snapshot() {
		ids[it.id+"/"+it.dir] = true
	}
	assert.Equal(t, map[string]bool{"b/upload": true, "a/download": true}, ids)

	v := r.Version()
	r.remove("b", status.DirUpload)
	r.remove("a", status.DirDownload)
	assert.NotEqual(t, v, r.Version())
	assert.Empty(t, r.snapshot())
	v = r.Version()
	r.remove("b", status.DirUpload) // unknown: nothing changed
	assert.Equal(t, v, r.Version())

	// a nil registry is a no-op
	var nilReg *transferRegistry
	nilReg.queue("x", status.DirUpload, "x", 1)
	nilReg.remove("x", status.DirUpload)
	assert.Empty(t, nilReg.snapshot())
}

func TestCountingWriter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var seen []uint64
	w := &countingWriter{w: &buf, report: func(n uint64) { seen = append(seen, n) }}
	w.Write([]byte("hello"))
	w.Write([]byte("world!"))
	assert.Equal(t, []uint64{5, 11}, seen)
}
