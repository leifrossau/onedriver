package main

import (
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
)

func TestStatusLineChecking(t *testing.T) {
	now := time.Date(2026, 5, 1, 18, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }

	// while checking, also when the daemon is too old to report State=syncing
	assert.Equal(t, "Checking for changes…",
		StatusLineAt(Instance{State: "syncing", Checking: true, LastSync: ago(time.Hour)}, now))
	assert.Equal(t, "Checking for changes…", StatusLineAt(Instance{State: "idle", Checking: true}, now))
	assert.Equal(t, StateSyncing, Instance{State: "idle", Checking: true}.state())
	// real transfers win over the checking text
	assert.Equal(t, "Syncing: 1 upload, 0 downloads",
		StatusLineAt(Instance{State: "syncing", Checking: true, PendingUploads: 1}, now))
	// offline is not hidden by a check
	assert.Equal(t, "Offline", StatusLineAt(Instance{State: "offline", Checking: true}, now))

	assert.Equal(t, "Up to date · checked just now",
		StatusLineAt(Instance{State: "idle", LastSync: ago(20 * time.Second)}, now))
	assert.Equal(t, "Up to date · checked 3 min ago",
		StatusLineAt(Instance{State: "idle", LastSync: ago(3*time.Minute + 5*time.Second)}, now))
	assert.Equal(t, "Up to date · checked 2 h ago",
		StatusLineAt(Instance{State: "idle", LastSync: ago(2 * time.Hour)}, now))
	assert.Equal(t, "Up to date · checked 3 d ago",
		StatusLineAt(Instance{State: "idle", LastSync: ago(72 * time.Hour)}, now))

	// the text ages with the clock; an unchanged text must not change the
	// menu signature (no LayoutUpdated)
	i := Instance{BusName: "x", MountPoint: "/m", State: "idle", LastSync: ago(10 * time.Second)}
	s1 := menuSignature(BuildMenuAt([]Instance{i}, now))
	assert.Equal(t, s1, menuSignature(BuildMenuAt([]Instance{i}, now.Add(20*time.Second))))
	assert.NotEqual(t, s1, menuSignature(BuildMenuAt([]Instance{i}, now.Add(2*time.Minute))))
}

func TestCheckingShowsInTray(t *testing.T) {
	e := newEnv(t)
	d := newFakeDaemon(t, e.addr, "home_u_C", "/home/u/C")
	d.start(t)
	assert.Eventually(t, func() bool { return len(e.tray.Instances()) == 1 }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "state-ok", e.sniProp(t, "IconName"))

	// the daemon reports Checking (and State=syncing with it)
	d.set("Checking", true)
	d.set("State", "syncing")
	assert.Eventually(t, func() bool { return e.sniProp(t, "IconName") == "state-sync" },
		3*time.Second, 20*time.Millisecond)
	assert.Eventually(t, func() bool {
		_, ok := e.layout(t).find(t, "Checking for changes…")
		return ok
	}, 3*time.Second, 20*time.Millisecond)

	d.set("Checking", false)
	d.set("State", "idle")
	d.set("LastSync", time.Now().Unix())
	assert.Eventually(t, func() bool { return e.sniProp(t, "IconName") == "state-ok" },
		3*time.Second, 20*time.Millisecond)
	assert.Eventually(t, func() bool {
		_, ok := e.layout(t).find(t, "Up to date · checked just now")
		return ok
	}, 3*time.Second, 20*time.Millisecond)
}

func TestParseStatusChecking(t *testing.T) {
	old := parseStatus("x", map[string]dbus.Variant{"State": dbus.MakeVariant("idle")})
	assert.False(t, old.Checking)
	nw := parseStatus("x", map[string]dbus.Variant{
		"State": dbus.MakeVariant("syncing"), "Checking": dbus.MakeVariant(true)})
	assert.True(t, nw.Checking)
}
