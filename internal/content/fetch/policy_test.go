package fetch

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestAllowHostIsExactAndNothingElse is the allowlist's actual semantics,
// written as the attacks it has to refuse.
//
// Every row that must fail is a real technique. A suffix match accepts
// "evil-steamcommunity.com"; a prefix match accepts
// "api.steampowered.com.evil.net"; a "subdomains are fine" rule accepts
// whatever a project page can register. There is no wildcard and no inference,
// and this table is what says so.
func TestAllowHostIsExactAndNothingElse(t *testing.T) {
	p := SteamPolicy()
	cases := []struct {
		host string
		want bool
		why  string
	}{
		{"steamcommunity.com", true, ""},
		{"api.steampowered.com", true, ""},
		// Normalisation, not inference: same host to a resolver, different
		// string here.
		{"STEAMCOMMUNITY.COM", true, "hostnames are case-insensitive"},
		{"steamcommunity.com.", true, "the DNS root is the same host"},
		{"steamcommunity.com:443", true, "a port is not part of the host"},

		{"evil-steamcommunity.com", false, "what a suffix match would accept"},
		{"api.steampowered.com.evil.net", false, "what a prefix match would accept"},
		{"sub.steamcommunity.com", false, "a subdomain is a different host"},
		{"steamcommunity.com.evil.net", false, ""},
		{"cdn.modrinth.com", false, "another source's host is not this source's"},
		{"127.0.0.1", false, ""},
		{"", false, ""},
		{"   ", false, ""},
		// Non-ASCII is refused outright, because CASE-FOLDING a homograph can
		// land on an allowlisted name. Two classes, and only one of them was
		// ever tested:
		//   - Cyrillic does NOT fold to ASCII, so this row passed for a reason
		//     unrelated to the matcher being safe. Kept, because it pins that
		//     class.
		{"steаmcommunity.com", false, "a Cyrillic а is not an a"},
		//   - U+0130 and U+212A DO fold: Unicode simple case mapping maps them
		//     to 'i' and 'k', and nine of the ten allowlisted hostnames
		//     contain an 'i'. strings.ToLower turned these into exact matches.
		{"apİ.steampowered.com", false, "U+0130 lowercases to i under Unicode case mapping"},
		{"steamcommunity.comK", false, "U+212A lowercases to k"},
	}
	for _, tc := range cases {
		if got := p.AllowHost(tc.host); got != tc.want {
			t.Errorf("AllowHost(%q) = %v, want %v %s", tc.host, got, tc.want, tc.why)
		}
	}
}

func TestTheShippedPoliciesAreHTTPSOnlyAndMatchSection124(t *testing.T) {
	cases := []struct {
		name  string
		p     Policy
		hosts []string
	}{
		{"steam", SteamPolicy(), []string{"api.steampowered.com", "steamcommunity.com"}},
		{"modrinth", ModrinthPolicy(), []string{
			"api.modrinth.com", "cdn.modrinth.com", "github.com",
			"raw.githubusercontent.com", "gitlab.com",
		}},
		{"curseforge", CurseForgePolicy(), []string{
			"api.curseforge.com", "edge.forgecdn.net", "mediafilez.forgecdn.net",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err != nil {
				t.Fatalf("the shipped policy is invalid: %v", err)
			}
			if !slices.Equal(tc.p.Hosts, tc.hosts) {
				t.Errorf("hosts = %v, want section 12.4's list %v", tc.p.Hosts, tc.hosts)
			}
			// The plain-HTTP escape hatch is Loopback's alone. A shipped policy
			// that permitted clear text would be fetching bytes that get
			// executed by a service account over a channel anyone on the path
			// can rewrite.
			if tc.p.allowPlainHTTP {
				t.Error("a shipped policy permits plain HTTP")
			}
			host := tc.hosts[0]
			if _, err := tc.p.CheckURL("http://" + host + "/x"); !errors.Is(err, ErrScheme) {
				t.Errorf("CheckURL(http://%s) = %v, want ErrScheme", host, err)
			}
			if _, err := tc.p.CheckURL("https://" + host + "/x"); err != nil {
				t.Errorf("CheckURL(https://%s) = %v", host, err)
			}
			if _, err := tc.p.CheckURL("https://evil.net/x"); !errors.Is(err, ErrHostNotAllowed) {
				t.Errorf("CheckURL(evil.net) = %v, want ErrHostNotAllowed", err)
			}
			if !strings.Contains(tc.p.String(), host) {
				t.Errorf("String() does not name the hosts: %s", tc.p.String())
			}
		})
	}
}

func TestCheckURLRefusesEveryUnfetchableShape(t *testing.T) {
	p := SteamPolicy()
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"an off-allowlist host", "https://evil.net/x", ErrHostNotAllowed},
		{"plain HTTP", "http://steamcommunity.com/x", ErrScheme},
		{"a file URL", "file:///etc/shadow", ErrScheme},
		{"an ftp URL", "ftp://steamcommunity.com/x", ErrScheme},
		{"credentials", "https://user:pw@steamcommunity.com/x", ErrCredentialsInURL},
		{"an opaque URL", "mailto:nobody@steamcommunity.com", ErrPolicy},
		{"no host", "https:///x", ErrPolicy},
		{"not a URL", "https://[::1", ErrPolicy},
		// The oldest confusion in the book. The allowlist catches it on its
		// own -- the host is evil.net -- and refusing userinfo outright means
		// nobody has to rely on that.
		{"a host-confusing authority", "https://api.steampowered.com@evil.net/x", ErrCredentialsInURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.CheckURL(tc.raw); !errors.Is(err, tc.want) {
				t.Fatalf("CheckURL(%q) = %v, want %v", tc.raw, err, tc.want)
			}
		})
	}
	u, err := p.CheckURL("  https://api.steampowered.com/ISteamRemoteStorage/?x=1  ")
	if err != nil {
		t.Fatalf("the control case was refused: %v", err)
	}
	if u.Host != "api.steampowered.com" {
		t.Errorf("Host = %q", u.Host)
	}
}

// TestEverySourceAManifestMayDeclareHasAnAllowlist is the bidirectional
// registry pairing, in the shape internal/spec uses for providers, hooks and
// resolvers.
//
// Both directions matter. A declarable source with no policy is a source whose
// fetcher either refuses every host or -- the failure that counts -- gets a
// hand-rolled client with no allowlist at all. A policy for a source no
// manifest can name is dead code that looks like a shipped feature.
func TestEverySourceAManifestMayDeclareHasAnAllowlist(t *testing.T) {
	declarable := spec.ContentSources()
	if len(declarable) == 0 {
		t.Fatal("spec.ContentSources() is empty, so this test proves nothing")
	}

	for name := range declarable {
		p, err := PolicyFor(name)
		if err != nil {
			t.Errorf("a manifest may declare source %q and there is no egress policy for it: "+
				"%v", name, err)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("the policy for %q is invalid: %v", name, err)
		}
	}
	for _, name := range PolicySources() {
		if _, ok := declarable[name]; !ok {
			t.Errorf("there is an egress policy for %q and no manifest can declare it; a "+
				"policy nothing can reach is dead code that looks like a feature", name)
		}
	}

	if _, err := PolicyFor("github_releases"); !errors.Is(err, ErrNoSource) {
		t.Errorf("PolicyFor(unknown) = %v, want ErrNoSource; a permissive default here is the "+
			"\"no allowlist\" a caller reaches for next", err)
	}
}

// TestLoopbackCannotBePointedAtAPublicHost bounds the one plain-HTTP escape
// hatch in the package. It exists for this package's own tests and for an
// operator running a local mirror; it must not be usable as a hole in the
// allowlist.
func TestLoopbackCannotBePointedAtAPublicHost(t *testing.T) {
	allowed := []string{
		"127.0.0.1", "127.0.0.53", "::1", "[::1]", "localhost",
		"api.localhost", "cdn.test", "off.test", "nothing.invalid",
	}
	for _, h := range allowed {
		if err := Loopback(h).Validate(); err != nil {
			t.Errorf("Loopback(%q) = %v, want a usable policy", h, err)
		}
	}
	refused := []string{
		"api.steampowered.com", "evil.net", "localhost.evil.net",
		"10.0.0.1", "192.168.1.1", "0.0.0.0", "test",
	}
	for _, h := range refused {
		if err := Loopback(h).Validate(); !errors.Is(err, ErrPolicy) {
			t.Errorf("Loopback(%q) was accepted; clear-text egress to a non-loopback host is "+
				"not a thing gvm does", h)
		}
	}
	// And it does permit what it is for.
	if _, err := Loopback("cdn.test").CheckURL("http://cdn.test/x"); err != nil {
		t.Errorf("Loopback refused its own scheme: %v", err)
	}
}

func TestNormalizeHostHandlesTheSpellingsAResolverTreatsAsOne(t *testing.T) {
	cases := map[string]string{
		"Example.COM":      "example.com",
		"example.com.":     "example.com",
		"example.com:443":  "example.com",
		"[::1]:8080":       "::1",
		"[::1]":            "::1",
		"  example.com  ":  "example.com",
		"example.com:":     "example.com",
		"127.0.0.1:1":      "127.0.0.1",
		"":                 "",
		"   ":              "",
		"api.test:0":       "api.test",
		"sub.example.com.": "sub.example.com",
		// Non-ASCII normalises to "" rather than to an ASCII lookalike. Every
		// other row here is ASCII, which is why the twelve of them could not
		// catch the folding bug.
		"apİ.steampowered.com": "",
		"Key":                  "",
		"EXAMPLE.com":          "example.com",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheDefaultBoundsAreTheOnesSection124Needs(t *testing.T) {
	// Not a tautology: these numbers are load-bearing and a casual edit to
	// them is a behaviour change. 16 MiB fits the biggest honest response
	// (a 500-item Steam collection page) and is small enough that a hostile or
	// gzip-bombed one cannot take out a 2 GB VPS.
	if DefaultMaxResponseBytes != 16<<20 {
		t.Errorf("DefaultMaxResponseBytes = %d", DefaultMaxResponseBytes)
	}
	if DefaultMaxRedirects < 1 || DefaultMaxRedirects > 10 {
		t.Errorf("DefaultMaxRedirects = %d", DefaultMaxRedirects)
	}
	if formatBytes(DefaultMaxResponseBytes) != "16MiB" {
		t.Errorf("formatBytes = %q", formatBytes(DefaultMaxResponseBytes))
	}
	// Modrinth publishes 300 requests per minute, which is five per second.
	// Sitting at or above it is how a sync earns a 429 on every run.
	if r := ModrinthPolicy().Rate; r >= 5 {
		t.Errorf("the Modrinth rate is %v/s and the published limit is 5/s", r)
	}
}

// TestAllowHostRefusesANonASCIIHostOutright is the matcher half of the
// homograph bug, and TestNormalizeHostHandlesTheSpellingsAResolverTreatsAsOne
// could not catch it: all twelve of its rows are ASCII.
//
// strings.ToLower applies Unicode SIMPLE CASE MAPPING, under which U+0130
// (LATIN CAPITAL LETTER I WITH DOT ABOVE) lowercases to 'i' and U+212A (KELVIN
// SIGN) to 'k'. Nine of the ten hostnames on the three shipped allowlists
// contain an 'i', so a host that is not on the list matched it -- and all three
// enforcement layers inherit the answer, because CheckURL, checkRedirect and
// dialAllowed all call AllowHost.
//
// Refusing raw non-ASCII costs nothing: a legitimate IDN reaches a Go HTTP
// client already punycoded, every allowlisted hostname is ASCII, and so is
// every test host including the loopback literals.
func TestAllowHostRefusesANonASCIIHostOutright(t *testing.T) {
	folding := map[string]string{
		"İ": "i", // LATIN CAPITAL LETTER I WITH DOT ABOVE
		"K": "k", // KELVIN SIGN
	}
	for r, ascii := range folding {
		if got := strings.ToLower(r); got != ascii {
			t.Fatalf("the premise moved: strings.ToLower(%q) = %q, want %q", r, got, ascii)
		}
	}

	for _, p := range []Policy{SteamPolicy(), ModrinthPolicy(), CurseForgePolicy()} {
		for _, host := range p.Hosts {
			for r, ascii := range folding {
				if !strings.Contains(host, ascii) {
					continue
				}
				spoof := strings.Replace(host, ascii, r, 1)
				if p.AllowHost(spoof) {
					t.Errorf("AllowHost(%q) is true and %q is not on the allowlist %v",
						spoof, spoof, p.Hosts)
				}
				if got := normalizeHost(spoof); got != "" {
					t.Errorf("normalizeHost(%q) = %q; a non-ASCII host must not normalise "+
						"into an allowlisted name", spoof, got)
				}
			}
			// The control: the real host still matches, in any case.
			if !p.AllowHost(strings.ToUpper(host)) {
				t.Errorf("AllowHost(%q) is false; plain ASCII case folding must still work",
					strings.ToUpper(host))
			}
		}
	}
}

// TestARefusalNamesTheHostAsItWasGiven is the audit half of the same bug, and
// it is a defect on its own: the three refusals printed normalizeHost(host), so
// an attempt on apİ.steampowered.com was recorded as "refused to dial
// api.steampowered.com" -- a host gvm never contacted. In a subsystem whose
// whole purpose is section 12.5's evidence trail, a record that names the wrong
// host is the wrong record.
//
// %q and not %s, so a homograph appears escaped in the record instead of
// rendering as a lookalike that reads as the real host.
func TestARefusalNamesTheHostAsItWasGiven(t *testing.T) {
	const spoof = "apİ.steampowered.com"
	p := SteamPolicy()

	_, err := p.CheckURL("https://" + spoof + "/x")
	if err == nil {
		t.Fatal("CheckURL accepted a host that is not on the allowlist")
	}
	if !strings.Contains(err.Error(), strconv.Quote(spoof)) {
		t.Errorf("CheckURL's refusal does not name the host as given, quoted: %v", err)
	}
	if strings.Contains(err.Error(), " api.steampowered.com (") {
		t.Errorf("CheckURL's refusal names a host gvm never contacted: %v", err)
	}
}
