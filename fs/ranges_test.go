package fs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRangeSetAdd(t *testing.T) {
	t.Parallel()
	var s rangeSet
	s = s.add(10, 20)
	s = s.add(30, 40)
	assert.Equal(t, rangeSet{{10, 20}, {30, 40}}, s)
	s = s.add(20, 25) // touching: merged
	assert.Equal(t, rangeSet{{10, 25}, {30, 40}}, s)
	s = s.add(0, 5)
	assert.Equal(t, rangeSet{{0, 5}, {10, 25}, {30, 40}}, s)
	s = s.add(3, 35) // spans several
	assert.Equal(t, rangeSet{{0, 40}}, s)
	s = s.add(50, 50) // empty: ignored
	assert.Equal(t, rangeSet{{0, 40}}, s)
	s = s.add(45, 60)
	assert.Equal(t, rangeSet{{0, 40}, {45, 60}}, s)
	assert.EqualValues(t, 55, s.total())
}

func TestRangeSetContainsMissing(t *testing.T) {
	t.Parallel()
	s := rangeSet{{0, 10}, {20, 30}}
	assert.True(t, s.contains(0, 10))
	assert.True(t, s.contains(22, 25))
	assert.True(t, s.contains(5, 5))
	assert.False(t, s.contains(5, 15))
	assert.False(t, s.contains(10, 20))
	assert.False(t, s.contains(25, 35))

	assert.Equal(t, rangeSet{{10, 20}}, s.missing(0, 30))
	assert.Equal(t, rangeSet{{10, 20}, {30, 40}}, s.missing(5, 40))
	assert.Nil(t, s.missing(21, 29))
	assert.Equal(t, rangeSet{{40, 50}}, s.missing(40, 50))
	assert.Equal(t, rangeSet{{0, 5}}, rangeSet(nil).missing(0, 5))
}

func TestPartialRecordJSON(t *testing.T) {
	t.Parallel()
	rec := partialRecord{Hash: "abc", Size: 100, Ranges: rangeSet{{0, 10}, {50, 60}}}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.JSONEq(t, `{"hash":"abc","size":100,"ranges":[[0,10],[50,60]]}`, string(data))
	var back partialRecord
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, rec, back)
}
