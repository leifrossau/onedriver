package dbus

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCollapsePaths(t *testing.T) {
	// dedupe, order kept
	assert.Equal(t, []string{"a/b", "c"}, CollapsePaths([]string{"a/b", "c", "a/b"}, 500))

	// overflow: unique parent folders
	var paths []string
	for i := 0; i < 600; i++ {
		paths = append(paths, fmt.Sprintf("dir%d/file%d", i%3, i))
	}
	paths = append(paths, "top.txt")
	assert.ElementsMatch(t, []string{"dir0", "dir1", "dir2", ""}, CollapsePaths(paths, 500))

	// still too many: the mount root
	paths = nil
	for i := 0; i < 600; i++ {
		paths = append(paths, fmt.Sprintf("dir%d/file", i))
	}
	assert.Equal(t, []string{""}, CollapsePaths(paths, 500))
}
