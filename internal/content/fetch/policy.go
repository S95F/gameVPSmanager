package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The egress refusals. Each is a sentinel because the retry ladder asks which
// of them can ever change its mind, and the answer for every one of them is no.
var (
	// ErrHostNotAllowed is the allowlist refusal: gvm was asked to talk to a
	// host this source may not talk to. TERMINAL -- retrying an allowlist is
	// how a ladder turns a policy decision into a loop.
	ErrHostNotAllowed = errors.New("fetch: host is not on this source's allowlist")
	// ErrScheme is a URL that is not https (and not a loopback http URL under
	// a Loopback policy). TERMINAL.
	ErrScheme = errors.New("fetch: URL scheme is not permitted")
	// ErrCredentialsInURL is a URL carrying userinfo. TERMINAL. Credentials in
	// a URL get written to the job log and to the audit params, and section
	// 12.4 says a signed URL is redacted to its path before it is recorded --
	// which is impossible to honour if the secret is in the host part.
	ErrCredentialsInURL = errors.New("fetch: URL carries credentials")
	// ErrRedirect is a redirect gvm will not follow: off the allowlist, or too
	// many. TERMINAL.
	ErrRedirect = errors.New("fetch: redirect refused")
	// ErrTooLarge is a response body that exceeded its bound. TERMINAL: an
	// unbounded read of a hostile response is an OOM, and asking again gets
	// the same response.
	ErrTooLarge = errors.New("fetch: response exceeds its size bound")
	// ErrPolicy is a malformed policy or a malformed URL: a bug in gvm or in a
	// manifest, not a network condition.
	ErrPolicy = errors.New("fetch: invalid fetch policy")
	// ErrNoSource is a source kind with no egress policy.
	ErrNoSource = errors.New("fetch: no egress policy for this content source")
)

// The per-source hostname allowlists of section 12.4, verbatim.
//
// These bound GVM'S OWN traffic and nothing else. They do not bound a malicious
// mod's traffic once the game is running -- that is section 18.4's declared
// residual risk -- and pretending otherwise is how a real mitigation gets
// treated as a solved problem.
//
// Exact hostnames, no wildcards, and the matcher does not do suffix matching.
// A suffix match on "steamcommunity.com" would accept
// "evil-steamcommunity.com"; a prefix match would accept
// "api.steampowered.com.evil.net". Both are one typo away from being written by
// somebody who wanted to allow a subdomain.
var (
	// SteamHosts is the Workshop's keyless surface: the Web API for
	// GetPublishedFileDetails and GetCollectionDetails, and the community site
	// for the HTML collection fallback.
	SteamHosts = []string{
		"api.steampowered.com",
		"steamcommunity.com",
	}
	// ModrinthHosts includes the forges because Modrinth's file list can point
	// a download at a project's own release asset.
	ModrinthHosts = []string{
		"api.modrinth.com",
		"cdn.modrinth.com",
		"github.com",
		"raw.githubusercontent.com",
		"gitlab.com",
	}
	// CurseForgeHosts is the API plus its two CDN hostnames. Being on this
	// list does NOT license synthesizing a CDN URL: section 12.8's build gate
	// refuses a derived edge.forgecdn.net path, and the only URLs fetched here
	// are ones a response handed over.
	CurseForgeHosts = []string{
		"api.curseforge.com",
		"edge.forgecdn.net",
		"mediafilez.forgecdn.net",
	}
)

// userAgent identifies gvm to the three APIs it uses.
//
// Steam's keyless endpoints and Modrinth both ask for a contactable agent
// string, and an anonymous Go default is what gets a shared IP rate-limited for
// everyone. The version is appended by the client.
const userAgent = "gvm"

// Default bounds, chosen for the biggest honest case rather than the typical
// one.
const (
	// DefaultMaxResponseBytes bounds an API or HTML response. A 500-item Steam
	// collection page is a few megabytes of HTML and a batched
	// GetPublishedFileDetails for the same collection is about one megabyte of
	// JSON, so 16 MiB is generous for everything this fetches and still small
	// enough that a hostile or gzip-bombed response cannot take out a 2 GB
	// VPS. The bound is applied to the DECODED stream, which is the only place
	// it can defend against a compression bomb.
	DefaultMaxResponseBytes int64 = 16 << 20
	// DefaultTimeout bounds one attempt's headers. It is not a download
	// budget: Stream's budget is the job's context, because a 2 GB modpack
	// legitimately takes longer than any number that could be hard-coded here.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRedirects is generous for a CDN chain and finite, which is the
	// property that matters: an infinite chain is a hang, and a hang inside a
	// download cycle is a server that is stopped.
	DefaultMaxRedirects = 5
)

// Policy is one content source's egress contract: where gvm may talk, how fast,
// how much it will read, and how often it will try again.
//
// It is a value, so a client cannot be re-pointed after construction, and the
// test seams are unexported fields, so no exported API can turn plain HTTP on
// for a public host.
type Policy struct {
	// Hosts is the hard hostname allowlist, enforced on the initial URL, on
	// EVERY redirect, and again in DialContext. Three layers, because the
	// first two are policy and the third is the one that still holds if a
	// future caller reaches for the transport directly.
	Hosts []string

	// UserAgent identifies gvm. Required: an anonymous agent is what gets a
	// shared IP throttled for every other operator on it.
	UserAgent string

	// MaxResponseBytes bounds a bodied response. Required and non-zero: the
	// zero value would read an unbounded hostile body, so there is no spelling
	// of this struct that means "no limit".
	MaxResponseBytes int64

	// Timeout bounds one attempt's response headers, not its body.
	Timeout time.Duration

	// MaxRedirects bounds a redirect chain. Zero means "do not follow any",
	// which is a meaningful policy and not an unset field, so Validate checks
	// the upper bound instead.
	MaxRedirects int

	// Rate and Burst are the token bucket's refill rate in requests per second
	// and its depth.
	Rate  float64
	Burst int

	// Retry is the bounded ladder. See retry.go.
	Retry Retry

	// allowPlainHTTP permits http:// for this policy. It is unexported and
	// Loopback is the only constructor that sets it, and Loopback refuses any
	// host that is not a loopback literal or a reserved test name -- so there
	// is no sequence of exported calls that produces a policy willing to send
	// a request in clear text to a public host.
	allowPlainHTTP bool

	// dial is the dialer the allowlist guards. Unexported, nil in production
	// (meaning a plain net.Dialer), and set only by this package's own tests,
	// which need two distinct hostnames pointed at one httptest listener to
	// prove the redirect refusal. The guard runs either way: the allowlist is
	// checked and THEN this is called.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// SteamPolicy is the Steam Workshop's egress contract.
//
// The rate is deliberately slower than Steam's (undocumented) ceiling. The
// keyless endpoints are a courtesy, a collection sync is a handful of batched
// calls rather than a per-item storm, and the cost of being slow here is
// seconds on an operation that already budgets an hour for a download cycle.
func SteamPolicy() Policy {
	return Policy{
		Hosts:            SteamHosts,
		UserAgent:        userAgent,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Timeout:          DefaultTimeout,
		MaxRedirects:     DefaultMaxRedirects,
		Rate:             2,
		Burst:            5,
		Retry:            DefaultRetry(),
	}
}

// ModrinthPolicy is Modrinth's egress contract. Modrinth publishes 300
// requests per minute with headers, so this sits under it at four per second
// and the client honours a 429's Retry-After on top.
func ModrinthPolicy() Policy {
	return Policy{
		Hosts:            ModrinthHosts,
		UserAgent:        userAgent,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Timeout:          DefaultTimeout,
		MaxRedirects:     DefaultMaxRedirects,
		Rate:             4,
		Burst:            8,
		Retry:            DefaultRetry(),
	}
}

// CurseForgePolicy is CurseForge's egress contract. CurseForge publishes no
// rate limit, which is a reason to pick a conservative number rather than a
// reason to pick none.
func CurseForgePolicy() Policy {
	return Policy{
		Hosts:            CurseForgeHosts,
		UserAgent:        userAgent,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Timeout:          DefaultTimeout,
		MaxRedirects:     DefaultMaxRedirects,
		Rate:             4,
		Burst:            8,
		Retry:            DefaultRetry(),
	}
}

// policies is the closed registry, keyed by the manifest's own `source:`
// strings so that a declared source and its allowlist cannot drift.
var policies = map[string]func() Policy{
	spec.SourceSteamWorkshop: SteamPolicy,
	spec.SourceModrinth:      ModrinthPolicy,
	spec.SourceCurseForge:    CurseForgePolicy,
}

// PolicyFor returns one source's egress contract.
//
// It refuses an unknown name rather than returning a permissive default,
// because the default a caller would reach for next is "no allowlist".
func PolicyFor(source string) (Policy, error) {
	if f, ok := policies[strings.TrimSpace(source)]; ok {
		return f(), nil
	}
	return Policy{}, fmt.Errorf("%w: %q (known: %s)", ErrNoSource, source,
		strings.Join(PolicySources(), ", "))
}

// PolicySources lists the sources that have an egress policy, sorted. The
// pairing with spec.ContentSources is asserted in this package's test.
func PolicySources() []string {
	out := make([]string, 0, len(policies))
	for k := range policies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Loopback is the policy for a server on this machine: this package's own
// tests, and an operator running a local mirror.
//
// It is the only constructor that permits plain HTTP, and it refuses any host
// that is not a loopback literal or a name in one of the reserved
// never-resolvable namespaces. So the escape hatch exists, it is visible, and
// it cannot be pointed at a public host -- which is what keeps "we needed it
// for the tests" from becoming a hole in the allowlist.
func Loopback(hosts ...string) Policy {
	p := ModrinthPolicy()
	p.Hosts = append([]string(nil), hosts...)
	p.allowPlainHTTP = true
	// Fast and deterministic: a test that waited out a real backoff would be a
	// test somebody disables.
	p.Rate = 1000
	p.Burst = 100
	return p
}

// loopbackHost reports whether h is somewhere only this machine can be.
//
// 127.0.0.0/8 and ::1 are the literals; .localhost, .test and .invalid are
// reserved by RFC 6761 and RFC 2606 and must never resolve publicly. Anything
// else is refused, which is what bounds Loopback's plain-HTTP permission.
func loopbackHost(h string) bool {
	h = normalizeHost(h)
	if h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	if h == "localhost" {
		return true
	}
	for _, suffix := range []string{".localhost", ".test", ".invalid"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// Validate refuses a policy that cannot be enforced.
//
// Every check here is a field whose zero value would be the dangerous reading:
// no hosts is "talk to anyone", zero MaxResponseBytes is "read forever", zero
// Rate is "as fast as the loop goes".
func (p Policy) Validate() error {
	if len(p.Hosts) == 0 {
		return fmt.Errorf("%w: no host allowlist; an empty list reads as \"any host\" to a "+
			"matcher and as \"no egress\" to the person who wrote it", ErrPolicy)
	}
	for _, h := range p.Hosts {
		switch {
		case strings.TrimSpace(h) == "":
			return fmt.Errorf("%w: an empty hostname in the allowlist", ErrPolicy)
		case strings.ContainsAny(h, "*?/\\ "):
			return fmt.Errorf("%w: %q is not a hostname; the matcher is exact and does no "+
				"wildcard or suffix matching, so a pattern here would never match anything",
				ErrPolicy, h)
		case strings.Contains(h, "://"):
			return fmt.Errorf("%w: %q looks like a URL; the allowlist holds hostnames",
				ErrPolicy, h)
		case strings.HasPrefix(h, "."):
			return fmt.Errorf("%w: %q starts with a dot; there is no suffix matching, so this "+
				"would match no host at all", ErrPolicy, h)
		case normalizeHost(h) == "":
			return fmt.Errorf("%w: %q normalises to nothing", ErrPolicy, h)
		}
		if p.allowPlainHTTP && !loopbackHost(h) {
			return fmt.Errorf("%w: %q is not a loopback or reserved-test host, and this policy "+
				"permits plain HTTP; clear-text egress to a public host is not a thing gvm does",
				ErrPolicy, h)
		}
	}
	switch {
	case strings.TrimSpace(p.UserAgent) == "":
		return fmt.Errorf("%w: no User-Agent; an anonymous agent is what gets a shared IP "+
			"throttled for every other operator on it", ErrPolicy)
	case p.MaxResponseBytes <= 0:
		return fmt.Errorf("%w: MaxResponseBytes is %d; an unbounded read of a hostile "+
			"response is an out-of-memory kill, so there is no spelling of this that means "+
			"\"no limit\"", ErrPolicy, p.MaxResponseBytes)
	case p.Timeout <= 0:
		return fmt.Errorf("%w: no timeout; a server that accepts a connection and never "+
			"answers would hang a download cycle with the game stopped", ErrPolicy)
	case p.MaxRedirects < 0:
		return fmt.Errorf("%w: MaxRedirects is %d", ErrPolicy, p.MaxRedirects)
	case p.MaxRedirects > 10:
		return fmt.Errorf("%w: MaxRedirects is %d; a chain that long is a loop somebody has "+
			"not noticed", ErrPolicy, p.MaxRedirects)
	case p.Rate <= 0:
		return fmt.Errorf("%w: Rate is %v requests per second; zero means \"as fast as the "+
			"loop goes\", which is how a keyless API stops being keyless", ErrPolicy, p.Rate)
	case p.Burst < 1:
		return fmt.Errorf("%w: Burst is %d, so the first request would never be granted",
			ErrPolicy, p.Burst)
	}
	return p.Retry.Validate()
}

// AllowHost reports whether this policy may talk to a host.
//
// EXACT match on the normalised hostname. No wildcards, no suffix matching, no
// subdomain inference:
//
//   - a suffix match on "steamcommunity.com" accepts "evil-steamcommunity.com"
//   - a prefix match accepts "api.steampowered.com.evil.net"
//   - "steamcommunity.com." is the same host to a resolver and a different
//     string here, so the trailing dot is normalised away rather than left to
//     fail closed by accident
//   - a host carrying any non-ASCII byte is REFUSED OUTRIGHT, rather than
//     lowercased and compared. Case-folding a homograph can land on an
//     allowlisted name: Unicode simple case mapping sends U+0130 to 'i' and
//     U+212A to 'k', and nine of the ten hostnames on the shipped allowlists
//     contain an 'i'. A legitimate IDN reaches a Go HTTP client already
//     punycoded, so refusing raw non-ASCII costs nothing and is the only
//     spelling that makes "exact match" true.
func (p Policy) AllowHost(host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	for _, allowed := range p.Hosts {
		if normalizeHost(allowed) == h {
			return true
		}
	}
	return false
}

// normalizeHost reduces a host, with or without a port, to the string the
// allowlist compares, or "" for a host that cannot be one.
func normalizeHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	if bare, _, err := net.SplitHostPort(h); err == nil {
		h = bare
	}
	h = strings.Trim(h, "[]")
	h = strings.TrimSuffix(h, ".") // the DNS root: same host, different string
	return asciiLower(h)
}

// asciiLower lowercases a hostname, and returns "" if it is not ASCII.
//
// Deliberately NOT strings.ToLower. That applies Unicode simple case mapping,
// under which U+0130 folds to 'i' and U+212A folds to 'k' -- so
// "apİ.steampowered.com" normalised to "api.steampowered.com" and matched the
// allowlist exactly, in all three enforcement layers at once, because CheckURL,
// checkRedirect and dialAllowed all go through AllowHost.
//
// Refusing rather than transliterating: a legitimate internationalised domain
// arrives at a Go HTTP client already punycoded (net/http does not do IDNA, so
// the raw label would go to the resolver and fail anyway), every allowlisted
// hostname is ASCII, and no gvm source needs one. The Go resolver's own
// isDomainName would also reject these bytes on the deployment target, but that
// is an implementation detail of somebody else's package and not a guarantee
// this package may rest a security property on.
func asciiLower(h string) string {
	var b strings.Builder
	b.Grow(len(h))
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 0x80:
			return ""
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// CheckURL is the first of the three allowlist layers: the URL gvm was asked
// to fetch.
func (p Policy) CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrPolicy, raw, err)
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("%w: %q is not a hierarchical URL", ErrPolicy, raw)
	}
	if u.User != nil {
		// Two reasons, and either alone would be enough. Credentials in a URL
		// are written to the job log and the audit params, and section 12.4
		// requires a signed URL to be redacted to its path before it is
		// recorded -- which cannot be done if the secret is in the authority.
		// And "https://api.curseforge.com@evil.net/" is the oldest confusion
		// in the book; the allowlist catches it, and refusing userinfo
		// outright means nobody has to rely on that.
		return nil, fmt.Errorf("%w: %s; credentials belong in a header delivered through "+
			"LoadCredential, never in a URL", ErrCredentialsInURL, u.Redacted())
	}
	if err := p.checkScheme(u); err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q has no host", ErrPolicy, raw)
	}
	if !p.AllowHost(u.Host) {
		// The host AS GIVEN, quoted. normalizeHost returns "" for a host it
		// refuses outright, so printing the normalised form would name
		// nothing -- and before that it named the host the attacker was
		// IMITATING rather than the one gvm was asked for. %q so a homograph
		// shows up escaped in the record instead of rendering as a lookalike
		// that reads as the real host.
		return nil, fmt.Errorf("%w: %q (allowed: %s)", ErrHostNotAllowed,
			u.Host, strings.Join(p.Hosts, ", "))
	}
	return u, nil
}

func (p Policy) checkScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if p.allowPlainHTTP {
			return nil
		}
		return fmt.Errorf("%w: %s is plain HTTP; a mod archive fetched in clear text is a mod "+
			"archive anyone on the path can replace, and gvm hands these bytes to a service "+
			"account that executes them", ErrScheme, u.Redacted())
	default:
		return fmt.Errorf("%w: %q", ErrScheme, u.Scheme)
	}
}

// String is the one-line description `gvm doctor` prints for a source's egress.
func (p Policy) String() string {
	s := fmt.Sprintf("hosts %s; %.0f req/s burst %d; <=%s per response; %d attempt(s)",
		strings.Join(p.Hosts, ","), p.Rate, p.Burst,
		formatBytes(p.MaxResponseBytes), p.Retry.MaxAttempts)
	if p.allowPlainHTTP {
		s += "; PLAIN HTTP (loopback only)"
	}
	return s
}

// formatBytes is a one-line size for a message. internal/spec has the real
// parser and formatter for manifest values; this is not one of those, and
// importing a systemd-suffix formatter to print "16MiB" in an error string
// would tie a diagnostic to a schema.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.0fGiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKiB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
