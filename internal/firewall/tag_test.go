package firewall_test

import (
	"testing"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

func TestParseTag(t *testing.T) {
	cases := []struct {
		comment string
		wantOK  bool
		wantTag firewall.Tag
	}{
		{"gvm:pz-main:game", true, firewall.Tag{Instance: "pz-main", Label: "game"}},
		{"gvm:host:ssh", true, firewall.Tag{Instance: "host", Label: "ssh"}},
		{"gvm:host:http", true, firewall.Tag{Instance: "host", Label: "http"}},
		{"wireguard", false, firewall.Tag{}},
		{"", false, firewall.Tag{}},
		{"gvm::", false, firewall.Tag{}},
		{"GVM:host:ssh", false, firewall.Tag{}}, // must be lowercase
		{"gvm:host:SSH", false, firewall.Tag{}}, // must be lowercase
	}
	for _, c := range cases {
		t.Run(c.comment, func(t *testing.T) {
			got, ok := firewall.ParseTag(c.comment)
			if ok != c.wantOK {
				t.Errorf("ParseTag(%q) ok = %v, want %v", c.comment, ok, c.wantOK)
			}
			if ok && got != c.wantTag {
				t.Errorf("ParseTag(%q) = %+v, want %+v", c.comment, got, c.wantTag)
			}
		})
	}
}

func TestTagRoundTrip(t *testing.T) {
	tag := firewall.Tag{Instance: "pz-main", Label: "game"}
	s := tag.String()
	got, ok := firewall.ParseTag(s)
	if !ok {
		t.Fatalf("ParseTag(%q) returned false", s)
	}
	if got != tag {
		t.Errorf("round trip: got %+v, want %+v", got, tag)
	}
}
