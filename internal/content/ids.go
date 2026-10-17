package content

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Validating an acquisition id at `gvm content add`, before it is written.
//
// Each source names its items differently -- Steam Workshop by a numeric id,
// Modrinth by a slug, CurseForge by a numeric project id -- and an operator
// reaches for whichever of the id, a URL, or (wrongly) a mod's display name is
// in front of them. Taking the string as-is and failing at the next sync, with
// a 404 from an API, tells them nothing about which of the things they typed was
// wrong. NormalizeID turns the forms gvm understands into the canonical id and
// refuses the rest here, with a sentence, where the operator still has the
// context to fix it.

// ErrNotAnID is an input that is not a valid acquisition id for its source. One
// sentinel for every form of the mistake: a caller asking "was this the
// operator's fault" wants one answer.
var ErrNotAnID = errors.New("content: not a valid acquisition id for this source")

var (
	reDigits       = regexp.MustCompile(`^[0-9]+$`)
	reModrinthSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)
	reModrinthVer  = regexp.MustCompile(`^[A-Za-z0-9]{4,}$`)
)

// NormalizeID validates input against src and returns the canonical id gvm
// stores. It accepts the bare id or the obvious URL for the source, and refuses
// anything else. An unknown source is not second-guessed: the trimmed input is
// returned, which is the behaviour from before this gate existed.
func NormalizeID(src SourceKind, input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", fmt.Errorf("%w: the id is empty", ErrNotAnID)
	}
	switch src {
	case KindSteamWorkshop:
		return normalizeWorkshopID(s)
	case KindModrinth:
		return normalizeModrinthID(s)
	case KindCurseForge:
		return normalizeCurseForgeID(s)
	default:
		return s, nil
	}
}

func normalizeWorkshopID(s string) (string, error) {
	if u, ok := asURL(s); ok {
		if !strings.Contains(u.Host, "steamcommunity.com") {
			return "", fmt.Errorf("%w: %s is not a Steam Workshop URL", ErrNotAnID, safeURL(u))
		}
		if id := u.Query().Get("id"); reDigits.MatchString(id) {
			return id, nil
		}
		return "", fmt.Errorf("%w: %s names no numeric ?id=", ErrNotAnID, safeURL(u))
	}
	if reDigits.MatchString(s) {
		return s, nil
	}
	// The common wrong input: a mod's display name, or an activation id from the
	// Mods= line, neither of which is the numeric item id gvm fetches by.
	return "", fmt.Errorf("%w: %q is not a Steam Workshop item id (an item id is the number in the "+
		"item's URL). It looks like an activation id — a Mods= entry — not an item to add; "+
		"`gvm content why %s` names the item it belongs to", ErrNotAnID, s, s)
}

func normalizeModrinthID(s string) (string, error) {
	if u, ok := asURL(s); ok {
		if !strings.Contains(u.Host, "modrinth.com") {
			return "", fmt.Errorf("%w: %s is not a Modrinth URL", ErrNotAnID, safeURL(u))
		}
		// modrinth.com/<type>/<slug>, where <type> is mod, plugin, datapack, ...
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 && reModrinthSlug.MatchString(parts[1]) {
			return parts[1], nil
		}
		return "", fmt.Errorf("%w: %s names no project slug (want modrinth.com/mod/<slug>)", ErrNotAnID, safeURL(u))
	}
	slug, ver, hasVer := strings.Cut(s, ":")
	if !reModrinthSlug.MatchString(slug) {
		return "", fmt.Errorf("%w: %q is not a Modrinth slug (lowercase letters, digits, - and _)", ErrNotAnID, slug)
	}
	if hasVer {
		if !reModrinthVer.MatchString(ver) {
			return "", fmt.Errorf("%w: %q is not a Modrinth version id", ErrNotAnID, ver)
		}
		return slug + ":" + ver, nil
	}
	return slug, nil
}

func normalizeCurseForgeID(s string) (string, error) {
	if u, ok := asURL(s); ok {
		if !strings.Contains(u.Host, "curseforge.com") {
			return "", fmt.Errorf("%w: %s is not a CurseForge URL", ErrNotAnID, safeURL(u))
		}
		// A CurseForge project URL is slug-based and does not carry the numeric
		// project id gvm pins by, so there is nothing to extract.
		return "", fmt.Errorf("%w: a CurseForge URL names a project by slug, not by the numeric id "+
			"gvm pins by. Use the Project ID from the project page: "+
			"gvm content add <instance> <projectID>[:<fileID>]", ErrNotAnID)
	}
	proj, file, hasFile := strings.Cut(s, ":")
	if !reDigits.MatchString(proj) {
		return "", fmt.Errorf("%w: %q is not a CurseForge project id (a number); add it as "+
			"<projectID>[:<fileID>]", ErrNotAnID, proj)
	}
	if hasFile && !reDigits.MatchString(file) {
		return "", fmt.Errorf("%w: %q is not a CurseForge file id (a number)", ErrNotAnID, file)
	}
	return s, nil
}

// asURL reports whether s is an http(s) URL, and parses it. A bare id or slug
// is not a URL and takes the other path.
func asURL(s string) (*url.URL, bool) {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, false
	}
	return u, true
}

// safeURL is the host and path only. A refusal message reaches a log, and a
// query string can carry anything, so it never goes in.
func safeURL(u *url.URL) string { return u.Host + u.Path }
