package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAggregateState(t *testing.T) {
	mk := func(states ...string) []Instance {
		var l []Instance
		for _, s := range states {
			l = append(l, Instance{State: s})
		}
		return l
	}
	assert.Equal(t, StateNone, AggregateState(nil))
	assert.Equal(t, StateIdle, AggregateState(mk("idle", "idle")))
	assert.Equal(t, StatePaused, AggregateState(mk("idle", "paused")))
	assert.Equal(t, StateSyncing, AggregateState(mk("paused", "syncing", "idle")))
	assert.Equal(t, StateOffline, AggregateState(mk("syncing", "offline", "paused")))
	assert.Equal(t, StateError, AggregateState(mk("offline", "error", "syncing")))
}

func TestIconAndStatus(t *testing.T) {
	assert.Equal(t, "state-error", IconName(StateError))
	assert.Equal(t, "state-offline", IconName(StateOffline))
	assert.Equal(t, "state-sync", IconName(StateSyncing))
	assert.Equal(t, "state-pause", IconName(StatePaused))
	assert.Equal(t, "state-ok", IconName(StateIdle))
	assert.Equal(t, "NeedsAttention", SNIStatus(StateError))
	assert.Equal(t, "Active", SNIStatus(StateSyncing))
}

func TestStatusLine(t *testing.T) {
	assert.Equal(t, "Up to date", StatusLine(Instance{State: "idle"}))
	assert.Equal(t, "Syncing: 3 uploads, 1 download",
		StatusLine(Instance{State: "syncing", PendingUploads: 3, PendingDownloads: 1}))
	assert.Equal(t, "Paused", StatusLine(Instance{State: "paused"}))
	assert.Equal(t, "Offline", StatusLine(Instance{State: "offline"}))
	assert.Equal(t, "Error: boom", StatusLine(Instance{State: "error", LastError: "boom"}))
}

func TestTooltip(t *testing.T) {
	now := time.Date(2026, 5, 1, 18, 0, 0, 0, time.Local)
	ts := time.Date(2026, 5, 1, 17, 42, 0, 0, time.Local).Unix()
	_, desc := Tooltip([]Instance{{Account: "a@b.c", State: "syncing", PendingUploads: 2,
		PendingDownloads: 5, LastSync: ts}}, now)
	assert.Contains(t, desc, "a@b.c")
	assert.Contains(t, desc, "2 uploads, 5 downloads pending")
	assert.Contains(t, desc, "Last sync: 17:42")
	_, desc = Tooltip([]Instance{{MountPoint: "/m"}}, now)
	assert.Contains(t, desc, "Last sync: never")
	_, desc = Tooltip(nil, now)
	assert.Contains(t, desc, "not running")
}

func TestHumanBytes(t *testing.T) {
	assert.Equal(t, "10 B", HumanBytes(10))
	assert.Equal(t, "1.5 KiB", HumanBytes(1536))
	assert.Equal(t, "5.0 GiB", HumanBytes(5<<30))
	assert.Equal(t, "Used 1.0 GiB of 5.0 GiB", QuotaText(Instance{QuotaUsed: 1 << 30, QuotaTotal: 5 << 30}))
	assert.Equal(t, "", QuotaText(Instance{}))
}

func labels(items []*MenuItem) []string {
	var l []string
	for _, it := range items {
		if it.Separator {
			l = append(l, "---")
		} else {
			l = append(l, it.Label)
		}
	}
	return l
}

func find(items []*MenuItem, label string) *MenuItem {
	for _, it := range items {
		if it.Label == label {
			return it
		}
	}
	return nil
}

func TestMenuNoInstances(t *testing.T) {
	m := BuildMenu(nil)
	assert.NotNil(t, find(m, "Open onedriver settings"))
	assert.Equal(t, ActSettings, find(m, "Open onedriver settings").Action.Kind)
	assert.NotNil(t, find(m, "Quit tray"))
	assert.Nil(t, find(m, "Open folder"))
}

func TestMenuSingleInstance(t *testing.T) {
	i := Instance{BusName: "x.A", MountPoint: "/home/u/OneDrive", Account: "u@x.y", State: "syncing",
		PendingUploads: 3, PendingDownloads: 2, QuotaUsed: 1 << 30, QuotaTotal: 5 << 30,
		Recent: []RecentItem{{"docs/a.txt", 1, "uploaded"}, {"old.txt", 2, "deleted"}}}
	m := BuildMenu([]Instance{i})
	idx := AssignIDs(m)
	assert.Equal(t, "Show activity", m[0].Label)
	assert.Equal(t, ActActivity, m[0].Action.Kind)
	assert.Equal(t, "/home/u/OneDrive - u@x.y", m[1].Label)
	assert.True(t, m[1].Disabled)
	assert.NotNil(t, find(m, "Syncing: 3 uploads, 2 downloads"))
	assert.NotNil(t, find(m, "Used 1.0 GiB of 5.0 GiB"))
	assert.Equal(t, &Action{ActOpenFolder, "x.A", "/home/u/OneDrive"}, find(m, "Open folder").Action)
	assert.Equal(t, ActPause, find(m, "Pause syncing").Action.Kind)
	assert.Equal(t, ActSyncNow, find(m, "Sync now").Action.Kind)
	assert.Equal(t, ActResync, find(m, "Refresh from OneDrive").Action.Kind)
	rec := find(m, "Recent files")
	assert.Len(t, rec.Children, 2)
	assert.Equal(t, "docs/a.txt (uploaded)", rec.Children[0].Label)
	assert.Equal(t, "/home/u/OneDrive/docs/a.txt", rec.Children[0].Action.Arg)
	assert.True(t, rec.Children[1].Disabled)
	assert.Equal(t, "Settings…", m[len(m)-2].Label)
	assert.Equal(t, "Quit tray", m[len(m)-1].Label)
	// ids are unique and indexed
	assert.Equal(t, rec.Children[0], idx[rec.Children[0].ID])

	i.State, i.Paused = "paused", true
	assert.NotNil(t, find(BuildMenu([]Instance{i}), "Resume syncing"))
}

func TestMenuMultipleInstances(t *testing.T) {
	m := BuildMenu([]Instance{
		{BusName: "b", MountPoint: "/b", State: "idle"},
		{BusName: "a", MountPoint: "/a", State: "offline"},
	})
	assert.Equal(t, []string{"Show activity", "/a - Offline", "/b - Up to date", "---", "Settings…", "Quit tray"}, labels(m))
	assert.NotNil(t, find(m[1].Children, "Open folder"))
	assert.Equal(t, "a", find(m[1].Children, "Sync now").Action.Instance)
}

func TestComposeCoalesces(t *testing.T) {
	one := compose([]notifEvent{{"conflict", "dir/a (conflicted).txt", "", "/m", "inst"}})
	assert.Len(t, one, 1)
	assert.Equal(t, "Conflict in a (conflicted).txt: kept both versions", one[0].Body)

	many := compose([]notifEvent{
		{"conflict", "a", "", "/m", "inst"}, {"conflict", "b", "", "/m", "inst"}, {"conflict", "c", "", "/m", "inst"},
		{"auth-expired", "", "", "/m", "inst"}, {"auth-expired", "", "", "/m", "inst"},
	})
	assert.Len(t, many, 2)
	assert.Contains(t, many[0].Body, "3 conflicts")
	assert.True(t, many[1].Login)
	assert.Equal(t, "inst", many[1].Instance)

	failed := compose([]notifEvent{{"upload-failed", "x.bin", "too big", "/m", "inst"}})
	assert.Contains(t, failed[0].Body, "x.bin")
	assert.Contains(t, failed[0].Body, "too big")
}
