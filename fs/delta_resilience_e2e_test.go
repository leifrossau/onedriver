package fs

// Tests for the delta loop's behaviour when the server or the network misbehaves
// (the situation after a laptop resumes from suspend), and for new remote items
// showing up in already-cached folders. They need the mock Graph server.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/status"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastSync returns the time of the last successful delta poll.
func lastSync(f *Filesystem) time.Time {
	f.ctl.mu.Lock()
	defer f.ctl.mu.Unlock()
	return f.ctl.lastSync
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A response that is not a proper delta page (captive portal, truncated body, an
// error object with status 200) used to replace deltaLink with "", after which
// every poll silently succeeded without ever returning a change again.
func TestPollDeltasKeepsLinkOnBadResponse(t *testing.T) {
	t.Parallel()
	_, cache, unmount := mountMockAtOpts(t, filepath.Join(testDBLoc, "e2e_"+t.Name()), false)
	defer unmount()

	good := cache.deltaLink
	require.NotEmpty(t, good)
	for _, fault := range []string{"html", "empty", "error", "truncated"} {
		link := good + "&fault=" + fault
		cache.deltaLink = link
		_, _, err := cache.pollDeltas(auth)
		assert.Error(t, err, "%s response was accepted", fault)
		assert.Equal(t, link, cache.deltaLink, "%s response clobbered deltaLink", fault)
	}

	// and a good response afterwards still works and keeps a usable link
	cache.deltaLink = good
	_, cont, err := cache.pollDeltas(auth)
	require.NoError(t, err)
	assert.False(t, cont)
	assert.NotEmpty(t, cache.deltaLink)
}

// After a long sleep the server may refuse the old delta token with 410
// resyncRequired. Retrying the same link forever left the filesystem "offline"
// and blind to changes, now the loop starts over and catches up.
func TestDeltaLoopRecoversFromExpiredToken(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_resync"
	mock.Mkdir(remoteDir)
	mnt, cache, unmount := mountMockAtOpts(t, filepath.Join(testDBLoc, "e2e_"+t.Name()), false)
	defer unmount()
	dir := filepath.Join(mnt, remoteDir)
	require.Empty(t, dirNames(t, dir)) // caches the (empty) listing

	// the file arrives while we are "away", and our token went stale
	mock.PutFile(remoteDir+"/added_while_away.txt", []byte("hi"))
	cache.deltaLink += "&fault=expired"
	go cache.DeltaLoop(500 * time.Millisecond)

	require.Eventually(t, func() bool {
		for _, n := range dirNames(t, dir) {
			if n == "added_while_away.txt" {
				return true
			}
		}
		return false
	}, 20*time.Second, 200*time.Millisecond, "resync did not bring in the new file")
	assert.GreaterOrEqual(t, mock.DeltaFaultHits("expired"), 1)
	assert.False(t, cache.IsOffline())
	assert.NotContains(t, cache.deltaLink, "expired")
}

// "Sync now" must make the loop poll right away, whether it is sleeping between
// polls, backing off after failures, or was clicked in the middle of a poll.
func TestSyncNowWakesDeltaLoop(t *testing.T) {
	t.Parallel()
	_, cache, unmount := mountMockAtOpts(t, filepath.Join(testDBLoc, "e2e_"+t.Name()), false)
	defer unmount()

	// a very long interval, so only SyncNow can cause the next poll
	go cache.DeltaLoop(time.Hour)
	require.Eventually(t, func() bool { return !lastSync(cache).IsZero() },
		10*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // the loop is asleep now
	t0 := lastSync(cache)
	cache.SyncNow()
	require.Eventually(t, func() bool { return lastSync(cache).After(t0) },
		3*time.Second, 20*time.Millisecond, "SyncNow did not trigger a poll")
}

// While polls keep failing the loop backs off, but SyncNow still retries at once.
func TestSyncNowCutsBackoffShort(t *testing.T) {
	t.Parallel()
	_, cache, unmount := mountMockAtOpts(t, filepath.Join(testDBLoc, "e2e_"+t.Name()), false)
	defer unmount()

	cache.deltaLink += "&fault=html" // every poll fails and leaves the link alone
	go cache.DeltaLoop(time.Hour)
	require.Eventually(t, func() bool { return mock.DeltaFaultHits("html") >= 1 },
		10*time.Second, 20*time.Millisecond)
	require.Eventually(t, cache.IsOffline, 2*time.Second, 20*time.Millisecond)

	// the waits would be 2s, 4s, 8s: three more polls in a second is only possible
	// through SyncNow
	for want := 2; want <= 4; want++ {
		cache.SyncNow()
		require.Eventually(t, func() bool { return mock.DeltaFaultHits("html") >= want },
			1*time.Second, 20*time.Millisecond, "SyncNow did not cut the backoff short")
	}
}

// A SyncNow that arrives while a poll is running is remembered and starts
// another poll right after it (instead of being lost, and the poll that was
// already in flight possibly missing what made the user click).
func TestSyncNowDuringPoll(t *testing.T) {
	t.Parallel()
	_, cache, unmount := mountMockAtOpts(t, filepath.Join(testDBLoc, "e2e_"+t.Name()), false)
	defer unmount()

	cache.deltaLink += "&fault=slow" // first poll takes 700ms, then the link is clean
	go cache.DeltaLoop(time.Hour)
	require.Eventually(t, func() bool { return mock.DeltaFaultHits("slow") >= 1 },
		5*time.Second, 10*time.Millisecond)
	cache.SyncNow() // the first poll is in flight

	seen := map[time.Time]bool{}
	require.Eventually(t, func() bool {
		if ls := lastSync(cache); !ls.IsZero() {
			seen[ls] = true
		}
		return len(seen) >= 2
	}, 5*time.Second, 10*time.Millisecond, "no second poll after SyncNow during a poll")
}

// Files added on the server to a folder that was listed before show up, and are
// reported in the activity list.
func TestDeltaNewFileInCachedFolder(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_newfile"
	dir, cache := mountMockFS(t, remoteDir)
	require.Empty(t, dirNames(t, dir))

	mock.PutFile(remoteDir+"/from_desktop.txt", []byte("new"))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "from_desktop.txt"))
		return err == nil
	}, 20*time.Second, 200*time.Millisecond)

	found := false
	for _, a := range cache.Recent(50) {
		if a.Action == status.ActionAdded && strings.HasSuffix(a.Path, "from_desktop.txt") {
			found = true
		}
	}
	assert.True(t, found, "no activity entry for the new remote file")
}

// A delta for a folder that was never listed must not make its children list
// look complete: the folder would then only show the one new file.
func TestDeltaNewFileInUnlistedFolder(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_unlisted"
	mock.Mkdir(remoteDir + "/sub")
	mock.PutFile(remoteDir+"/sub/old.txt", []byte("old"))
	dir, cache := mountMockFS(t, remoteDir)
	require.Equal(t, []string{"sub"}, dirNames(t, dir)) // sub itself is not listed

	mock.PutFile(remoteDir+"/sub/new.txt", []byte("new"))
	t0 := lastSync(cache)
	require.Eventually(t, func() bool { return lastSync(cache).Sub(t0) > 1500*time.Millisecond },
		20*time.Second, 100*time.Millisecond) // a few polls went by

	assert.ElementsMatch(t, []string{"old.txt", "new.txt"}, dirNames(t, filepath.Join(dir, "sub")))
}
