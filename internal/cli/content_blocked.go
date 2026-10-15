// `gvm content blocked` — the AWAITING_MANUAL_FILES report of section 12.8.
//
// CurseForge lets an author opt out of API distribution. When they have,
// `downloadUrl` comes back null (or 403) and the file CANNOT be fetched by any
// legitimate means: the CDN URL shape is trivially derivable, which is exactly
// why synthesizing it is circumvention of the author's expressed choice, a
// terms breach, and how an operator loses their API key. There is a build gate
// in internal/content/curseforge asserting gvm never does it.
//
// So the resolver records the file as BLOCKED and continues -- it never aborts
// on the first blocked mod -- and the consolidated list lands in
// /var/lib/gvm/instances/<name>/blocked-mods.json. This command prints it. That
// is Prism Launcher's "download these yourself" modal, headless.
//
// Two honest statements the command makes rather than implying:
//
//   - A steam_workshop block cannot produce a blocked item at all. The Workshop
//     has no distribution opt-out, and the server downloads its own items. So
//     "none" for a Project Zomboid instance is a fact about the source and not
//     a reading of a file, and saying which it is matters: an operator chasing
//     a missing mod needs to know this command cannot be the answer.
//   - The file itself is written by the resolver. Its ABSENCE means no sync has
//     recorded blocked files, not that there are none upstream.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func runContentBlocked(ctx context.Context, env *Env, args []string) error {
	fset, opt := contentFlags(env, "blocked", "<instance>")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return flagErr(err)
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}
	return printBlocked(ctx, env, v, opt.JSON)
}

// blockedReport is the shape of blocked-mods.json.
//
// It is decoded into a declared struct rather than printed through: a file
// under /var/lib is written by gvm and read back by gvm, and a passthrough
// would make this command a renderer for whatever happened to be in it.
type blockedReport struct {
	Instance string        `json:"instance"`
	Block    string        `json:"block,omitempty"`
	State    string        `json:"state,omitempty"`
	Files    []blockedFile `json:"files"`
	// Reason is this command's own sentence when there is no file to read. It
	// is carried in the JSON too, so a dashboard distinguishes "nothing is
	// blocked" from "nothing has looked".
	Reason string `json:"reason,omitempty"`
}

type blockedFile struct {
	Name      string            `json:"name"`
	Project   string            `json:"project,omitempty"`
	AcquireID content.AcquireID `json:"acquire_id,omitempty"`
	SHA1      string            `json:"sha1,omitempty"`
	Size      int64             `json:"size,omitempty"`
	// URL is the author's own download page, which a HUMAN can open. It is
	// never a CDN URL gvm derived.
	URL string `json:"url,omitempty"`
}

func printBlocked(_ context.Context, env *Env, v *contentView, asJSON bool) error {
	rep := blockedReport{Instance: v.Name, Block: v.Schema.Block}

	switch {
	case v.Schema.Source == content.KindSteamWorkshop:
		rep.Reason = "a Steam Workshop block cannot have blocked items: the Workshop has no " +
			"distribution opt-out, and this block's mechanism is " +
			v.Schema.Acquisition.Mechanism + ", so the server downloads its own items. " +
			"An item that has not arrived is reported by `gvm content diff` as not on disk."
	default:
		raw, err := v.Host.files().ReadFile(v.Paths.BlockedMods)
		switch {
		case err == nil:
			if jerr := json.Unmarshal(raw, &rep); jerr != nil {
				return fmt.Errorf("%s: %w", v.Paths.BlockedMods, jerr)
			}
			rep.Instance, rep.Block = v.Name, v.Schema.Block
		case errors.Is(err, fs.ErrNotExist):
			rep.Reason = fmt.Sprintf("%s does not exist, so no sync has recorded a blocked "+
				"file for this instance. That is not the same as none being blocked upstream",
				v.Paths.BlockedMods)
		default:
			return err
		}
	}

	if asJSON {
		return writeJSON(env.Stdout, rep)
	}
	printBlockedReport(env.Stdout, v, rep)
	return nil
}

func printBlockedReport(w io.Writer, v *contentView, rep blockedReport) {
	fmt.Fprintf(w, "\n  %s   block %s (%s)\n", v.Name, rep.Block, v.Schema.Source)
	if len(rep.Files) == 0 {
		fmt.Fprintf(w, "\n  0 files require a manual download.\n  %s\n", rep.Reason)
		return
	}
	fmt.Fprintf(w, "\n  %s require a manual download (the author opted out of API "+
		"distribution):\n\n", plural(len(rep.Files), "file"))
	for _, f := range rep.Files {
		fmt.Fprintf(w, "    %-22s %-34s %8s  sha1 %s\n", f.Project, f.Name,
			humanBytes(f.Size), shortSHA(f.SHA1))
		if f.URL != "" {
			fmt.Fprintf(w, "      %s\n", f.URL)
		}
	}
	fmt.Fprintf(w, "\n  Put them in /var/lib/gvm/downloads/mods, then re-run:\n"+
		"      gvm content blocked %s\n", v.Name)
	if rep.State != "" {
		fmt.Fprintf(w, "  state: %s\n", rep.State)
	}
	fmt.Fprint(w, "\n  gvm never derives a CDN URL for one of these. The URL shape is "+
		"trivially\n  guessable, which is precisely why using it against an opt-out is "+
		"circumvention\n  of the author's expressed choice and how an operator loses their "+
		"API key.\n")
}

// humanBytes is the size column. Two significant figures, because the column
// is for recognising a file in a browser's download list and not for
// accounting.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "—"
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	if s == "" {
		return "—"
	}
	return s
}
