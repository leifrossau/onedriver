package fs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newCheckFS() *Filesystem { return &Filesystem{ctl: newSyncControl()} }

func TestCheckingLifecycle(t *testing.T) {
	t.Parallel()
	f := newCheckFS()
	assert.False(t, f.Checking())

	// background polls and Resume/Login wake-ups do not announce a check
	f.wakeLoop()
	assert.False(t, f.Checking())
	f.endCheck(f.checkTicket())
	assert.False(t, f.Checking())

	// SyncNow -> true until the cycle has ended and the minimum time passed
	f.SyncNow()
	assert.True(t, f.Checking())
	f.endCheck(f.checkTicket()) // a poll that found nothing, instantly
	assert.True(t, f.Checking(), "must stay visible for a moment")
	assert.Eventually(t, func() bool { return !f.Checking() }, 3*time.Second, 20*time.Millisecond)
	select {
	case <-f.CheckingEvents():
	default:
		t.Error("no event for the change")
	}
}

func TestCheckingNewRequestDuringCycle(t *testing.T) {
	t.Parallel()
	f := newCheckFS()
	f.SyncNow()
	ticket := f.checkTicket() // cycle starts
	f.SyncNow()               // second request while it runs
	f.endCheck(ticket)
	time.Sleep(checkMinVisible + 100*time.Millisecond)
	assert.True(t, f.Checking(), "the next cycle has to serve the second request")
	f.endCheck(f.checkTicket())
	assert.Eventually(t, func() bool { return !f.Checking() }, 3*time.Second, 20*time.Millisecond)
}

func TestCheckingErrorAndPauseClear(t *testing.T) {
	t.Parallel()
	// a failed cycle ends the check like a successful one
	f := newCheckFS()
	f.SyncNow()
	f.endCheck(f.checkTicket())
	assert.Eventually(t, func() bool { return !f.Checking() }, 3*time.Second, 20*time.Millisecond)

	// Pause clears right away, also a Resync
	f.Resync()
	assert.True(t, f.Checking())
	f.Pause()
	assert.False(t, f.Checking())
}

func TestCheckingResyncNoTimeout(t *testing.T) {
	t.Parallel()
	f := newCheckFS()
	f.Resync()
	f.ctl.checkMu.Lock()
	f.ctl.checkSince = time.Now().Add(-10 * time.Minute)
	f.ctl.checkMu.Unlock()
	assert.True(t, f.Checking(), "a full enumeration may take minutes")

	g := newCheckFS()
	g.SyncNow()
	g.ctl.checkMu.Lock()
	g.ctl.checkSince = time.Now().Add(-checkTimeout - time.Second)
	g.ctl.checkMu.Unlock()
	assert.False(t, g.Checking(), "SyncNow times out")
}
