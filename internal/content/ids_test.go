package content

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeIDAcceptsTheFormsEachSourceUses(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  SourceKind
		in   string
		want string
	}{
		{"workshop bare id", KindSteamWorkshop, "2392709985", "2392709985"},
		{"workshop url", KindSteamWorkshop,
			"https://steamcommunity.com/sharedfiles/filedetails/?id=2392709985&searchtext=x", "2392709985"},
		{"modrinth slug", KindModrinth, "sodium", "sodium"},
		{"modrinth slug and version", KindModrinth, "sodium:Yp8wLY1P", "sodium:Yp8wLY1P"},
		{"modrinth url", KindModrinth, "https://modrinth.com/mod/sodium", "sodium"},
		{"curseforge project id", KindCurseForge, "238222", "238222"},
		{"curseforge project and file", KindCurseForge, "238222:3980857", "238222:3980857"},
		{"unknown source is taken as-is", SourceKind("other"), "  whatever  ", "whatever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeID(tc.src, tc.in)
			if err != nil {
				t.Fatalf("NormalizeID(%s, %q) errored: %v", tc.src, tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeID(%s, %q) = %q, want %q", tc.src, tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeIDRefusesWhatWouldFailAtSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  SourceKind
		in   string
	}{
		{"empty", KindSteamWorkshop, "   "},
		{"workshop display name / activation id", KindSteamWorkshop, "ThatDamnLibrary"},
		{"workshop foreign url", KindSteamWorkshop, "https://example.com/?id=1"},
		{"workshop url with no id", KindSteamWorkshop, "https://steamcommunity.com/sharedfiles/filedetails/"},
		{"modrinth bad slug", KindModrinth, "Sodium!"},
		{"modrinth empty version", KindModrinth, "sodium:"},
		{"curseforge non-numeric", KindCurseForge, "jei"},
		{"curseforge slug url", KindCurseForge, "https://www.curseforge.com/minecraft/mc-mods/jei"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeID(tc.src, tc.in)
			if !errors.Is(err, ErrNotAnID) {
				t.Fatalf("NormalizeID(%s, %q) = (%q, %v), want ErrNotAnID", tc.src, tc.in, got, err)
			}
		})
	}
}

// The activation-id refusal points the operator at the command that finds the
// item, rather than leaving them to guess what a Workshop id looks like.
func TestWorkshopActivationIDRefusalNamesContentWhy(t *testing.T) {
	_, err := NormalizeID(KindSteamWorkshop, "ThatDamnLibrary")
	if err == nil || !errors.Is(err, ErrNotAnID) {
		t.Fatalf("err = %v, want ErrNotAnID", err)
	}
	if got := err.Error(); !strings.Contains(got, "gvm content why") {
		t.Errorf("the refusal does not point at `gvm content why`: %q", got)
	}
}
