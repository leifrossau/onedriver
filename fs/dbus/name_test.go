package dbus

import (
	"testing"

	"github.com/jstaf/onedriver/fs/status"
)

func TestBusName(t *testing.T) {
	for in, want := range map[string]string{
		"/home/leif/OneDrive":  "io.github.onedriver.Mount.home_leif_OneDrive",
		"/home/leif/My Drive":  "io.github.onedriver.Mount.home_leif_My_x20Drive",
		"/mnt/1drive":          "io.github.onedriver.Mount.mnt_1drive",
		"/":                    "io.github.onedriver.Mount._",
		"/home/leif/a-b_c.d":   "io.github.onedriver.Mount.home_leif_a_x2db_c_d",
		"/home/leif/OneDrive/": "io.github.onedriver.Mount.home_leif_OneDrive",
	} {
		if got := BusName(in); got != want {
			t.Errorf("BusName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeKey(t *testing.T) {
	for in, want := range map[string]string{
		"1abc": "_1abc",
		"é":    "__",
		"":     "_",
		"a.b":  "a_b",
	} {
		if got := escapeKey(in); got != want {
			t.Errorf("escapeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveState(t *testing.T) {
	for _, c := range []struct {
		err, paused, offline bool
		up, down             uint32
		want                 string
	}{
		{false, false, false, 0, 0, status.StateIdle},
		{false, false, false, 1, 0, status.StateSyncing},
		{false, false, false, 0, 2, status.StateSyncing},
		{false, false, true, 1, 0, status.StateOffline},
		{false, true, true, 1, 1, status.StatePaused},
		{true, true, true, 1, 1, status.StateError},
		{true, false, false, 0, 0, status.StateError},
	} {
		if got := status.DeriveState(c.err, c.paused, c.offline, c.up, c.down); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}
