package fs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The locked Personal Vault (and packages like OneNote notebooks) have neither
// a file nor a folder facet. They must show up as empty, read-only
// directories and never be downloaded.
func TestOpaqueItemsAreReadOnlyDirs(t *testing.T) {
	mnt, _, unmount := mountMockAt(t, filepath.Join(testDBLoc, "e2e_opaque"))
	defer unmount()

	vaultID := mock.PutVault("Personlig boks")
	pkgID := mock.PutPackage("/opaque_pkg", "Notebook", "oneNote")
	require.NotEmpty(t, vaultID)
	require.NotEmpty(t, pkgID)
	t.Cleanup(func() { mock.Delete(vaultID) })

	// listing the root shows the vault as a directory
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(mnt, "Personlig boks"))
		return err == nil
	}, 10*1e9, 100*1e6)
	entries, err := os.ReadDir(mnt)
	require.NoError(t, err)
	found := false
	for _, e := range entries {
		if e.Name() == "Personlig boks" {
			found = true
			assert.True(t, e.IsDir())
		}
	}
	assert.True(t, found, "vault should be listed")

	for _, p := range []string{
		filepath.Join(mnt, "Personlig boks"),
		filepath.Join(mnt, "opaque_pkg", "Notebook"),
	} {
		st, err := os.Stat(p)
		require.NoError(t, err, p)
		assert.True(t, st.IsDir(), p)
		assert.Zero(t, st.Mode().Perm()&0222, "read-only: "+p)

		kids, err := os.ReadDir(p)
		require.NoError(t, err, p)
		assert.Empty(t, kids, p)

		// cannot write into it
		err = os.WriteFile(filepath.Join(p, "x.txt"), []byte("x"), 0644)
		require.Error(t, err, p)
		assert.ErrorIs(t, err, syscall.EROFS, p)
		assert.ErrorIs(t, os.Mkdir(filepath.Join(p, "d"), 0755), syscall.EROFS, p)
		// nor remove it
		assert.Error(t, os.Remove(p), p)
	}
	assert.Error(t, os.Rename(filepath.Join(mnt, "Personlig boks"), filepath.Join(mnt, "other")))

	// reading it as a file never reaches the content endpoint, and neither
	// did anything else
	_, err = os.ReadFile(filepath.Join(mnt, "Personlig boks"))
	assert.Error(t, err)
	assert.Zero(t, contentRequests(vaultID), "content endpoint must not be hit")
	assert.Zero(t, contentRequests(pkgID), "content endpoint must not be hit")
	assert.True(t, mock.Exists("/Personlig boks"), "vault must not be deleted server side")
}
