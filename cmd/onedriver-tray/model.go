package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Everything in this file is pure: no D-Bus, no I/O. It turns the state of the
// running onedriver instances into icon, tooltip and menu data.

// Aggregate states, ordered by precedence in AggregateState.
const (
	StateNone    = "none" // no onedriver instance is running
	StateIdle    = "idle"
	StatePaused  = "paused"
	StateSyncing = "syncing"
	StateOffline = "offline"
	StateError   = "error"
)

// RecentItem is one entry of the GetRecent() reply.
type RecentItem struct {
	Path   string
	Time   int64
	Action string
}

// Instance is the tray's view of one running onedriver mount.
type Instance struct {
	BusName          string
	MountPoint       string
	Account          string
	State            string
	Paused           bool
	PendingUploads   uint32
	PendingDownloads uint32
	QuotaUsed        uint64
	QuotaTotal       uint64
	LastSync         int64
	LastError        string
	Recent           []RecentItem
}

func (i Instance) paused() bool { return i.Paused || i.State == StatePaused }

// state returns the instance state, treating unknown values as idle.
func (i Instance) state() string {
	switch i.State {
	case StateIdle, StatePaused, StateSyncing, StateOffline, StateError:
		return i.State
	}
	return StateIdle
}

var statePrecedence = map[string]int{
	StateNone: 0, StateIdle: 1, StatePaused: 2, StateSyncing: 3, StateOffline: 4, StateError: 5,
}

// AggregateState picks the most important state: error > offline > syncing >
// paused > idle. No instances means StateNone.
func AggregateState(insts []Instance) string {
	best := StateNone
	for _, i := range insts {
		if s := i.state(); statePrecedence[s] > statePrecedence[best] {
			best = s
		}
	}
	return best
}

// IconName maps an aggregate state to a Breeze status icon.
func IconName(state string) string {
	switch state {
	case StateError:
		return "state-error"
	case StateOffline:
		return "state-offline"
	case StateSyncing:
		return "state-sync"
	case StatePaused:
		return "state-pause"
	case StateIdle:
		return "state-ok"
	}
	return "state-information"
}

// SNIStatus is the StatusNotifierItem Status for an aggregate state.
func SNIStatus(state string) string {
	if state == StateError {
		return "NeedsAttention"
	}
	return "Active"
}

func plural(n uint32, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// PendingText is e.g. "3 uploads, 1 download".
func PendingText(up, down uint32) string {
	return plural(up, "upload") + ", " + plural(down, "download")
}

// StatusLine is the one-line status of an instance.
func StatusLine(i Instance) string {
	switch i.state() {
	case StateError:
		if i.LastError != "" {
			return "Error: " + i.LastError
		}
		return "Error"
	case StateOffline:
		return "Offline"
	case StatePaused:
		return "Paused"
	case StateSyncing:
		return "Syncing: " + PendingText(i.PendingUploads, i.PendingDownloads)
	}
	return "Up to date"
}

// HumanBytes formats a byte count with binary units.
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	f := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for n, u := range units {
		f /= unit
		if f < unit || n == len(units)-1 {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return ""
}

// QuotaText is "Used X of Y", or "" when the quota is unknown.
func QuotaText(i Instance) string {
	if i.QuotaTotal == 0 {
		return ""
	}
	return fmt.Sprintf("Used %s of %s", HumanBytes(i.QuotaUsed), HumanBytes(i.QuotaTotal))
}

// FormatLastSync renders a unix time relative to now.
func FormatLastSync(ts int64, now time.Time) string {
	if ts <= 0 {
		return "never"
	}
	t := time.Unix(ts, 0).In(now.Location())
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// Name is the human-readable name of an instance.
func (i Instance) Name() string {
	if i.Account != "" {
		return i.Account
	}
	return i.MountPoint
}

// Tooltip returns the title and description of the tray tooltip.
func Tooltip(insts []Instance, now time.Time) (title, desc string) {
	if len(insts) == 0 {
		return "OneDrive", "onedriver is not running"
	}
	var lines []string
	for _, i := range sorted(insts) {
		line := fmt.Sprintf("%s: %s\n%s pending\nLast sync: %s",
			i.Name(), StatusLine(i), PendingText(i.PendingUploads, i.PendingDownloads),
			FormatLastSync(i.LastSync, now))
		lines = append(lines, line)
	}
	return "OneDrive", strings.Join(lines, "\n\n")
}

func sorted(insts []Instance) []Instance {
	out := append([]Instance(nil), insts...)
	sort.Slice(out, func(a, b int) bool {
		if out[a].MountPoint != out[b].MountPoint {
			return out[a].MountPoint < out[b].MountPoint
		}
		return out[a].BusName < out[b].BusName
	})
	return out
}

// Action kinds triggered by menu items.
const (
	ActOpenFolder = "open-folder"
	ActOpenFile   = "open-file"
	ActPause      = "pause"
	ActResume     = "resume"
	ActSyncNow    = "sync"
	ActSettings   = "settings"
	ActActivity   = "activity"
	ActQuit       = "quit"
)

// Action is what happens when a menu item is clicked.
type Action struct {
	Kind     string
	Instance string // bus name of the instance
	Arg      string // e.g. absolute file path
}

// MenuItem is a plain-data menu entry. ID is filled in by AssignIDs.
type MenuItem struct {
	ID        int32
	Label     string
	Separator bool
	Disabled  bool
	Icon      string
	Action    *Action
	Children  []*MenuItem
}

func item(label string, a *Action) *MenuItem { return &MenuItem{Label: label, Action: a} }
func info(label string) *MenuItem            { return &MenuItem{Label: label, Disabled: true} }
func sep() *MenuItem                         { return &MenuItem{Separator: true} }

// recentLabel is the menu label of a recent file.
func recentLabel(r RecentItem) string {
	return fmt.Sprintf("%s (%s)", r.Path, r.Action)
}

func instanceItems(i Instance) []*MenuItem {
	h := i.MountPoint
	if i.Account != "" {
		h += " - " + i.Account
	}
	m := []*MenuItem{info(h), info(StatusLine(i))}
	open := item("Open folder", &Action{Kind: ActOpenFolder, Instance: i.BusName, Arg: i.MountPoint})
	open.Icon = "folder-open"
	m = append(m, open)

	recent := &MenuItem{Label: "Recent files", Icon: "document-open-recent"}
	for _, r := range i.Recent {
		it := item(recentLabel(r), &Action{Kind: ActOpenFile, Instance: i.BusName,
			Arg: strings.TrimRight(i.MountPoint, "/") + "/" + strings.TrimLeft(r.Path, "/")})
		if r.Action == "deleted" {
			it.Action = nil
			it.Disabled = true
		}
		recent.Children = append(recent.Children, it)
	}
	if len(recent.Children) == 0 {
		recent.Children = []*MenuItem{info("No recent files")}
	}
	m = append(m, recent)

	if i.paused() {
		m = append(m, item("Resume syncing", &Action{Kind: ActResume, Instance: i.BusName}))
	} else {
		m = append(m, item("Pause syncing", &Action{Kind: ActPause, Instance: i.BusName}))
	}
	m = append(m, item("Sync now", &Action{Kind: ActSyncNow, Instance: i.BusName}))
	if q := QuotaText(i); q != "" {
		m = append(m, info(q))
	}
	return m
}

func activityItem() *MenuItem {
	it := item("Show activity", &Action{Kind: ActActivity})
	it.Icon = "view-list-details"
	return it
}

// BuildMenu builds the tray menu: one flat section for a single instance, one
// submenu per instance for several, plus the global entries.
func BuildMenu(insts []Instance) []*MenuItem {
	var m []*MenuItem
	insts = sorted(insts)
	switch len(insts) {
	case 0:
		m = append(m, info("onedriver is not running"),
			item("Open onedriver settings", &Action{Kind: ActSettings}))
	case 1:
		m = append(m, activityItem())
		m = append(m, instanceItems(insts[0])...)
	default:
		m = append(m, activityItem())
		for _, i := range insts {
			m = append(m, &MenuItem{
				Label:    fmt.Sprintf("%s - %s", i.MountPoint, StatusLine(i)),
				Children: instanceItems(i),
			})
		}
	}
	m = append(m, sep())
	if len(insts) > 0 {
		m = append(m, item("Settings…", &Action{Kind: ActSettings}))
	}
	m = append(m, item("Quit tray", &Action{Kind: ActQuit}))
	return m
}

// AssignIDs numbers the items depth-first starting at 1 (0 is the root) and
// returns an id -> item index.
func AssignIDs(items []*MenuItem) map[int32]*MenuItem {
	idx := map[int32]*MenuItem{}
	var next int32 = 1
	var walk func([]*MenuItem)
	walk = func(l []*MenuItem) {
		for _, it := range l {
			it.ID = next
			idx[next] = it
			next++
			walk(it.Children)
		}
	}
	walk(items)
	return idx
}
