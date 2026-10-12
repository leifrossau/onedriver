package main

import (
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	notifDst   = "org.freedesktop.Notifications"
	notifObj   = dbus.ObjectPath("/org/freedesktop/Notifications")
	loginKey   = "login"
	loginLabel = "Log in"
)

// notifEvent is one Notify signal received from an instance.
type notifEvent struct {
	Kind, Path, Message, Mount string
	Instance                   string // bus name of the sender
}

// notification is one desktop notification to show.
type notification struct {
	Summary string
	Body    string
	Icon    string
	Login   bool // offer a "Log in" action
	// Instance is the bus name of the instance to log in
	Instance string
}

// compose turns a burst of events into as few notifications as possible: one
// per (kind, mount) when there is a single event, one summary otherwise.
func compose(events []notifEvent) []notification {
	type key struct{ kind, mount string }
	var order []key
	groups := map[key][]notifEvent{}
	for _, e := range events {
		k := key{e.Kind, e.Mount}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	var out []notification
	for _, k := range order {
		g := groups[k]
		n := len(g)
		e := g[0]
		switch k.kind {
		case "conflict":
			if n == 1 {
				out = append(out, notification{"OneDrive", fmt.Sprintf("Conflict in %s: kept both versions", path.Base(e.Path)), "dialog-warning", false, ""})
			} else {
				out = append(out, notification{"OneDrive", fmt.Sprintf("%d conflicts in %s: kept both versions of each file", n, k.mount), "dialog-warning", false, ""})
			}
		case "upload-failed":
			if n == 1 {
				body := fmt.Sprintf("Upload of %s failed", path.Base(e.Path))
				if e.Message != "" {
					body += ": " + e.Message
				}
				out = append(out, notification{"OneDrive", body, "dialog-error", false, ""})
			} else {
				out = append(out, notification{"OneDrive", fmt.Sprintf("%d uploads failed in %s", n, k.mount), "dialog-error", false, ""})
			}
		case "auth-expired":
			out = append(out, notification{"OneDrive", "Your OneDrive login has expired. Please log in again.", "dialog-password", true, e.Instance})
		default:
			body := e.Message
			if n > 1 {
				body = fmt.Sprintf("%d errors in %s, latest: %s", n, k.mount, g[n-1].Message)
			}
			if body == "" {
				body = "An error occurred"
			}
			out = append(out, notification{"OneDrive", body, "dialog-error", false, ""})
		}
	}
	return out
}

// Notifier shows desktop notifications, coalescing bursts of events that
// arrive within a short window.
type Notifier struct {
	conn    *dbus.Conn
	window  time.Duration
	onLogin func(instance string)

	mu      sync.Mutex
	pending []notifEvent
	timer   *time.Timer
	login   map[uint32]string
}

func newNotifier(conn *dbus.Conn, window time.Duration, onLogin func(instance string)) *Notifier {
	return &Notifier{conn: conn, window: window, onLogin: onLogin, login: map[uint32]string{}}
}

// Add queues an event; the batch is sent once the window has elapsed.
func (n *Notifier) Add(e notifEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending = append(n.pending, e)
	if n.timer == nil {
		n.timer = time.AfterFunc(n.window, n.flush)
	}
}

func (n *Notifier) flush() {
	n.mu.Lock()
	events := n.pending
	n.pending, n.timer = nil, nil
	n.mu.Unlock()
	for _, nt := range compose(events) {
		n.send(nt)
	}
}

func (n *Notifier) send(nt notification) {
	actions := []string{}
	if nt.Login {
		actions = []string{loginKey, loginLabel}
	}
	hints := map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant("onedriver-tray")}
	call := n.conn.Object(notifDst, notifObj).Call(notifDst+".Notify", 0,
		"onedriver", uint32(0), nt.Icon, nt.Summary, nt.Body, actions, hints, int32(-1))
	if call.Err != nil {
		return
	}
	var id uint32
	if call.Store(&id) == nil && nt.Login {
		n.mu.Lock()
		n.login[id] = nt.Instance
		n.mu.Unlock()
	}
}

// ActionInvoked handles the org.freedesktop.Notifications.ActionInvoked signal.
func (n *Notifier) ActionInvoked(id uint32, key string) {
	n.mu.Lock()
	inst, ok := n.login[id]
	n.mu.Unlock()
	if ok && key == loginKey {
		n.onLogin(inst)
	}
}
