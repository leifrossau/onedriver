package dbus

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// FileURL spells the local file mountpoint/rel the way
// QUrl::fromLocalFile(...).toEncoded() does: everything except unreserved
// characters, sub-delimiters, ":", "@" and "/" is percent-encoded (non-ASCII
// as UTF-8 bytes). QUrl parses this form as well as the decoded one KIO sends.
func FileURL(mountpoint, rel string) string {
	full := path.Join(filepath.ToSlash(mountpoint), rel)
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(full); i++ {
		c := full[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("-._~!$&'()*+,;=:@/", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
