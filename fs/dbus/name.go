package dbus

import (
	"strings"

	"github.com/coreos/go-systemd/v22/unit"
)

// Names and paths of the exported interface, see docs/dbus-interface.md.
const (
	NamePrefix = "io.github.onedriver.Mount."
	ObjectPath = "/io/github/onedriver/Mount"
	Interface  = "io.github.onedriver.Mount1"
)

// BusName returns the well-known bus name for the instance serving the
// given (absolute) mountpoint: the mountpoint is escaped with
// systemd-escape --path rules, then every character outside [A-Za-z0-9_]
// becomes "_" and a leading digit gets a "_" prefix.
func BusName(mountpoint string) string {
	return NamePrefix + escapeKey(unit.UnitNamePathEscape(mountpoint))
}

func escapeKey(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_') // one per byte, bus names are ASCII
		}
	}
	key := b.String()
	if key == "" || (key[0] >= '0' && key[0] <= '9') {
		key = "_" + key
	}
	return key
}
