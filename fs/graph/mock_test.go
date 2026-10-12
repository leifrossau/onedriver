package graph

// Tests of the Graph client functions against the mock server. They need to
// mutate or inspect server-side state, so they are skipped on live accounts.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mockTestAuth(t *testing.T) *Auth {
	t.Helper()
	if mock == nil {
		t.Skip("needs the mock Graph server (unset ONEDRIVER_TEST_LIVE)")
	}
	return &Auth{
		AccessToken: "mock-access-token",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
	}
}

func TestMockMkdirRenameRemove(t *testing.T) {
	t.Parallel()
	auth := mockTestAuth(t)
	root, err := GetItem("root", auth)
	require.NoError(t, err)

	dir, err := Mkdir("mkdir-test", root.ID, auth)
	require.NoError(t, err)
	assert.True(t, dir.IsDir())
	_, err = Mkdir("mkdir-test", root.ID, auth)
	assert.Error(t, err, "creating an existing folder should conflict")
	assert.Contains(t, err.Error(), "nameAlreadyExists")

	require.NoError(t, Rename(dir.ID, "mkdir-renamed", root.ID, auth))
	_, err = GetItemPath("/mkdir-test", auth)
	assert.Error(t, err)
	renamed, err := GetItemPath("/mkdir-renamed", auth)
	require.NoError(t, err)
	assert.Equal(t, dir.ID, renamed.ID)

	require.NoError(t, Remove(dir.ID, auth))
	_, err = GetItem(dir.ID, auth)
	require.Error(t, err)
	assert.False(t, IsOffline(err), "an HTTP error is not an offline error")
}

func TestMockChildrenPaging(t *testing.T) {
	t.Parallel()
	auth := mockTestAuth(t)
	for i := 0; i < 450; i++ {
		mock.PutFile(fmt.Sprintf("/graph-paging/%d.txt", i), []byte("x"))
	}
	children, err := GetItemChildrenPath("/graph-paging", auth)
	require.NoError(t, err)
	assert.Len(t, children, 450)
}

func TestMockLargeContentDownload(t *testing.T) {
	t.Parallel()
	auth := mockTestAuth(t)
	// larger than one 10MB download chunk, so the Range-based path is used
	content := bytes.Repeat([]byte("0123456789abcdef"), 11*1024*1024/16+1)
	id := mock.PutFile("/graph-large/big.bin", content)

	got, n, err := GetItemContent(id, auth)
	require.NoError(t, err)
	assert.Equal(t, uint64(len(content)), n)
	assert.Equal(t, QuickXORHash(&content), QuickXORHash(&got))
}

func TestMockPutAndHash(t *testing.T) {
	t.Parallel()
	auth := mockTestAuth(t)
	mock.Mkdir("/graph-put")
	content := []byte("some content to hash")
	resp, err := Put(ResourcePath("/graph-put/new.txt")+":/content", auth, strings.NewReader(string(content)))
	require.NoError(t, err)
	assert.Contains(t, string(resp), `"quickXorHash"`)

	item, err := GetItemPath("/graph-put/new.txt", auth)
	require.NoError(t, err)
	assert.True(t, item.VerifyChecksum(QuickXORHash(&content)),
		"the mock must report the real QuickXorHash of the content")
	assert.NotEmpty(t, item.ETag)
}
