package firewall

import (
	"fmt"
	"regexp"
)

// Tag is gvm's ownership marker, carried in the ufw rule comment.
// Format: gvm:<instance>:<label>
// A rule without a parseable gvm tag is FOREIGN and gvm will NEVER delete it.
// Special instance "host" is used for host-wide rules (SSH, HTTP/S).
type Tag struct {
	Instance string // instance name or "host"
	Label    string // port label from the manifest
}

var tagRe = regexp.MustCompile(`^gvm:([a-z0-9][a-z0-9_-]{0,31}):([a-z0-9][a-z0-9_-]{0,31})$`)

// ParseTag parses the comment field of a rule.
// Returns (Tag, true) if valid, (zero, false) otherwise.
func ParseTag(comment string) (Tag, bool) {
	m := tagRe.FindStringSubmatch(comment)
	if m == nil {
		return Tag{}, false
	}
	return Tag{Instance: m[1], Label: m[2]}, true
}

// String renders the tag as it appears in a ufw comment.
func (t Tag) String() string {
	return fmt.Sprintf("gvm:%s:%s", t.Instance, t.Label)
}

// Valid returns true if both parts are non-empty and the tag regex matches.
func (t Tag) Valid() bool {
	return tagRe.MatchString(t.String())
}
