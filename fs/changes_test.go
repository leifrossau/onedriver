package fs

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChangeSet(t *testing.T) {
	var c changeSet
	c.noteID("a") // not recording yet
	ids, paths, overflow := c.take()
	assert.Empty(t, ids)
	assert.Empty(t, paths)
	assert.False(t, overflow)

	c.noteID("a")
	c.noteID("a")
	c.noteID("b")
	c.noteID("")
	c.notePath("x/y")
	c.notePath("x/y")
	ids, paths, overflow = c.take()
	assert.ElementsMatch(t, []string{"a", "b"}, ids)
	assert.Equal(t, []string{"x/y"}, paths)
	assert.False(t, overflow)

	// drained
	ids, paths, _ = c.take()
	assert.Empty(t, ids)
	assert.Empty(t, paths)

	for i := 0; i < maxPendingChanges+10; i++ {
		c.noteID(fmt.Sprint(i))
	}
	ids, _, overflow = c.take()
	assert.Len(t, ids, maxPendingChanges)
	assert.True(t, overflow)

	var nilSet *changeSet
	nilSet.noteID("x")
	nilSet.notePath("x")
}
