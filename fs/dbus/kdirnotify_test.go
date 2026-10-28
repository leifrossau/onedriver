package dbus

import (
	"fmt"
	"testing"

	"github.com/jstaf/onedriver/fs/status"
	"github.com/stretchr/testify/assert"
)

func TestFileURL(t *testing.T) {
	assert.Equal(t, "file:///home/u/OneDrive", FileURL("/home/u/OneDrive", ""))
	assert.Equal(t, "file:///home/u/OneDrive/a%20b/%C3%A5%23.txt", FileURL("/home/u/OneDrive", "a b/å#.txt"))
	assert.Equal(t, "file:///m/x%25y%3Fz", FileURL("/m", "x%y?z"))
	assert.Equal(t, "file:///m/a@b:c(1)+,;=.txt", FileURL("/m", "a@b:c(1)+,;=.txt"))
}

func TestCoalesceRemote(t *testing.T) {
	const m = "/mnt"
	ch := func(kind, p, n string) status.RemoteChange {
		return status.RemoteChange{Kind: kind, Path: p, NewPath: n}
	}

	// one FilesAdded per folder, duplicates dropped, the root is ""
	sig := CoalesceRemote(m, []status.RemoteChange{
		ch(status.RemoteAdded, "d", ""), ch(status.RemoteAdded, "d", ""), ch(status.RemoteAdded, "", ""),
		ch(status.RemoteRemoved, "d/x", ""), ch(status.RemoteRemoved, "d/x", ""),
		ch(status.RemoteRenamed, "d/a", "d/b"),
		ch(status.RemoteChanged, "d/c", ""), ch(status.RemoteChanged, "d/x", ""), // x is removed anyway
	})
	assert.Equal(t, []string{"file:///mnt/d", "file:///mnt"}, sig.Added)
	assert.Equal(t, []string{"file:///mnt/d/x"}, sig.Removed)
	assert.Equal(t, [][2]string{{"file:///mnt/d/a", "file:///mnt/d/b"}}, sig.Renamed)
	assert.Equal(t, []string{"file:///mnt/d/c"}, sig.Changed)
	assert.False(t, sig.Empty())
	assert.True(t, CoalesceRemote(m, nil).Empty())

	// too many removals: the parent folders are rescanned instead
	var many []status.RemoteChange
	for i := 0; i < maxKDirRemoved+1; i++ {
		many = append(many, ch(status.RemoteRemoved, fmt.Sprintf("d%d/f%d", i%3, i), ""))
	}
	sig = CoalesceRemote(m, many)
	assert.Empty(t, sig.Removed)
	assert.Equal(t, []string{"file:///mnt/d0", "file:///mnt/d1", "file:///mnt/d2"}, sig.Added)

	// too many changes / renames likewise
	many = nil
	for i := 0; i < maxKDirChanged+1; i++ {
		many = append(many, ch(status.RemoteChanged, fmt.Sprintf("x/f%d", i), ""))
	}
	for i := 0; i < maxKDirRenamed+1; i++ {
		many = append(many, ch(status.RemoteRenamed, fmt.Sprintf("a/f%d", i), fmt.Sprintf("b/f%d", i)))
	}
	sig = CoalesceRemote(m, many)
	assert.Empty(t, sig.Changed)
	assert.Empty(t, sig.Renamed)
	assert.Equal(t, []string{"file:///mnt/a", "file:///mnt/b", "file:///mnt/x"}, sig.Added)

	// too many folders collapse to their parents, then to the root
	many = nil
	for i := 0; i < maxKDirParents+1; i++ {
		many = append(many, ch(status.RemoteAdded, fmt.Sprintf("p/d%d", i), ""))
	}
	assert.Equal(t, []string{"file:///mnt/p"}, CoalesceRemote(m, many).Added)
	many = nil
	for i := 0; i < maxKDirParents+1; i++ {
		many = append(many, ch(status.RemoteAdded, fmt.Sprintf("p%d/d", i), ""))
	}
	assert.Equal(t, []string{"file:///mnt"}, CoalesceRemote(m, many).Added)
}
