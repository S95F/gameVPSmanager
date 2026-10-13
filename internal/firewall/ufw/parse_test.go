package ufw

import (
	"os"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

func loadFixture(t *testing.T, name string) ParseResult {
	t.Helper()
	f, err := os.Open("testdata/status/" + name)
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	defer f.Close()
	return Parse(f)
}

func TestParseActiveV4V6(t *testing.T) {
	res := loadFixture(t, "active-v4v6.txt")
	if !res.Active {
		t.Fatal("expected active")
	}
	if !res.Trusted {
		t.Fatal("expected trusted parse")
	}
	// v6 duplicates filtered: 5 logical rules
	if len(res.Rules) != 5 {
		t.Errorf("expected 5 rules (v6 deduped), got %d", len(res.Rules))
		for i, r := range res.Rules {
			t.Logf("  [%d] %+v", i, r)
		}
	}
	// Rule 0 must be 22/tcp LIMIT with gvm tag
	r := res.Rules[0]
	if r.Port != 22 || r.Proto != firewall.ProtoTCP || r.Verb != firewall.VerbLimit {
		t.Errorf("rule 0: expected 22/tcp LIMIT, got %+v", r)
	}
	tag, ok := firewall.ParseTag(r.Comment)
	if !ok {
		t.Errorf("rule 0 comment not a gvm tag: %q", r.Comment)
	} else if tag.Instance != "host" || tag.Label != "ssh" {
		t.Errorf("rule 0 tag: want host:ssh, got %s:%s", tag.Instance, tag.Label)
	}
}

func TestParseActiveNoIPv6(t *testing.T) {
	res := loadFixture(t, "active-no-ipv6.txt")
	if !res.Active {
		t.Fatal("expected active")
	}
	if !res.Trusted {
		t.Fatal("expected trusted")
	}
	if len(res.Rules) != 3 {
		t.Errorf("expected 3 rules, got %d", len(res.Rules))
	}
}

func TestParseInactive(t *testing.T) {
	res := loadFixture(t, "inactive.txt")
	if res.Active {
		t.Fatal("expected inactive")
	}
	if len(res.Rules) != 0 {
		t.Errorf("expected no rules, got %d", len(res.Rules))
	}
}

func TestParseCommentsWithHash(t *testing.T) {
	res := loadFixture(t, "comments-with-hash.txt")
	if !res.Active || !res.Trusted {
		t.Fatal("expected active and trusted")
	}
	// rule[1] has comment "manual rule #2" — the parser must not truncate at first #
	found := false
	for _, r := range res.Rules {
		if r.Port == 9000 {
			found = true
			if !containsHash(r.Comment) {
				t.Errorf("comment should contain '#', got %q", r.Comment)
			}
		}
	}
	if !found {
		t.Error("port 9000 rule not found")
	}
}

func containsHash(s string) bool {
	for _, c := range s {
		if c == '#' {
			return true
		}
	}
	return false
}

func TestParseRangesAndCIDRs(t *testing.T) {
	res := loadFixture(t, "ranges-and-cidrs.txt")
	if !res.Active || !res.Trusted {
		t.Fatal("expected active and trusted")
	}
	var rangeRule, cidrRule *firewall.Rule
	for i := range res.Rules {
		r := &res.Rules[i]
		if r.Port == 27000 {
			rangeRule = r
		}
		if r.Port == 22 && r.From != "" {
			cidrRule = r
		}
	}
	if rangeRule == nil {
		t.Fatal("range rule 27000:27030/udp not found")
	}
	if rangeRule.PortEnd != 27030 || rangeRule.Proto != firewall.ProtoUDP {
		t.Errorf("range rule: want Port=27000 PortEnd=27030 UDP, got %+v", *rangeRule)
	}
	if cidrRule == nil {
		t.Fatal("CIDR rule not found")
	}
	if cidrRule.From != "192.168.1.0/24" {
		t.Errorf("CIDR rule From: want 192.168.1.0/24, got %q", cidrRule.From)
	}
}

func TestParseCorruptUntrusted(t *testing.T) {
	res := loadFixture(t, "corrupt-untrusted.txt")
	if !res.Active {
		t.Fatal("expected active")
	}
	if res.Trusted {
		t.Error("expected Trusted=false for corrupt line")
	}
	// The valid rules should still be present.
	if len(res.Rules) < 2 {
		t.Errorf("expected at least 2 parseable rules, got %d", len(res.Rules))
	}
}

func TestParseFail2BanBans(t *testing.T) {
	res := loadFixture(t, "fail2ban-bans.txt")
	if !res.Active || !res.Trusted {
		t.Fatal("expected active and trusted")
	}
	// The REJECT IN <ip> rules have no port (To=Anywhere), they should parse.
	// fail2ban rules have From set — ParseTag will fail, so they'll be foreign.
	for _, r := range res.Rules {
		if r.From != "" {
			_, ok := firewall.ParseTag(r.Comment)
			if ok {
				t.Errorf("fail2ban rule unexpectedly has gvm tag: %+v", r)
			}
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	// Every gvm-tagged rule in active-v4v6 must have a valid ParseTag.
	res := loadFixture(t, "active-v4v6.txt")
	for _, r := range res.Rules {
		if r.Comment == "" {
			continue
		}
		tag, ok := firewall.ParseTag(r.Comment)
		if !ok {
			t.Errorf("gvm-tagged rule comment doesn't parse: %q", r.Comment)
			continue
		}
		if tag.String() != r.Comment {
			t.Errorf("tag round-trip mismatch: original=%q re-encoded=%q", r.Comment, tag.String())
		}
	}
}
