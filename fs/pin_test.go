package fs

import (
	"testing"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pin state is stored with the rest of the metadata, and databases
// written before pinning existed decode to "inherit".
func TestInodePinJSON(t *testing.T) {
	t.Parallel()
	old := []byte(`{"id":"abc","name":"old.txt","Children":null,"Subdir":0,"Mode":0}`)
	inode, err := NewInodeJSON(old)
	require.NoError(t, err)
	assert.Equal(t, pinInherit, inode.pin)
	assert.NotContains(t, string(inode.AsJSON()), `"pin"`, "inherit should not be stored")

	inode.pin = pinPinned
	restored, err := NewInodeJSON(inode.AsJSON())
	require.NoError(t, err)
	assert.Equal(t, pinPinned, restored.pin)
	assert.Equal(t, "old.txt", restored.Name())

	inode.pin = pinUnpinned
	restored, err = NewInodeJSON(inode.AsJSON())
	require.NoError(t, err)
	assert.Equal(t, pinUnpinned, restored.pin)
}

func TestParseBoolXAttr(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"1": true, "0": false, "true": true, "False": false, "1\n": true, "0\x00": false, "": false,
	} {
		got, ok := parseBoolXAttr([]byte(in))
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	_, ok := parseBoolXAttr([]byte("2"))
	assert.False(t, ok)
}

func TestServerPath(t *testing.T) {
	t.Parallel()
	item := func(parent, name string) *graph.DriveItem {
		return &graph.DriveItem{Name: name, Parent: &graph.DriveItemParent{Path: parent}}
	}
	assert.Equal(t, "/a.txt", serverPath(item("/drive/root:", "a.txt")))
	assert.Equal(t, "/dir/sub/a.txt", serverPath(item("/drive/root:/dir/sub", "a.txt")))
	assert.Equal(t, "/dir/a.txt", serverPath(item("/drives/xyz/root:/dir", "a.txt")))
	assert.Equal(t, "", serverPath(item("", "a.txt")))
	assert.Equal(t, "", serverPath(&graph.DriveItem{Name: "root"}))
}
