// `gvm content collection sync` — rebuild.sh step 1, upgraded.
//
// The bash fetched a Steam collection page, paired ids to titles with awk,
// counted the raw ids as a cross-check and refused to rewrite mods.txt when the
// two disagreed. That refusal is the reason this command exists as its own
// verb: a collection sync REWRITES THE OPERATOR'S LIST, and a list rewritten
// off a bad parse is indistinguishable from a list somebody shortened on
// purpose.
//
// Everything load-bearing is in internal/content/steamworkshop: the API path's
// per-child cross-checks, the HTML fallback's two different regexes, and
// Merge's rule that known items keep their POSITION while new ones are
// appended. This file is the part that is genuinely a command's -- which flags,
// which refusals before anything is fetched, and what the report says.
//
// It is the ONLY content command that makes a network request, and it says so
// in its own output. Everything else in this package is built Offline.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/steamworkshop"
)

func runContentCollection(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("gvm content collection sync <instance> is the only subcommand")
	}
	switch args[0] {
	case "sync":
		return runContentCollectionSync(ctx, env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, "Usage: gvm content collection sync <instance> "+
			"[--dry-run] [--no-fetch] [--allow-html-fallback]\n")
		return nil
	default:
		return usageErr(fmt.Errorf("unknown subcommand %q; `gvm content collection sync` is the only one",
			args[0]))
	}
}

func runContentCollectionSync(ctx context.Context, env *Env, args []string) error {
	fs, opt := contentFlags(env, "collection sync",
		"<instance> [--dry-run] [--no-fetch] [--allow-html-fallback]")
	dry := fs.Bool("dry-run", false, "print the merge and write nothing")
	noFetch := fs.Bool("no-fetch", false, "make no request at all; report what is declared")
	allowHTML := fs.Bool("allow-html-fallback", false,
		"scrape the collection page when the API reports the collection as private or removed")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}
	if v.Schema.Source != content.KindSteamWorkshop {
		return fmt.Errorf("block %q is a %s block and collections are a Steam Workshop concept",
			v.Schema.Block, v.Schema.Source)
	}
	// Both halves of the declaration are required, and they are refused
	// separately because the fixes are different files: the manifest says the
	// game HAS collections, the instance says WHICH one.
	if !v.Block.CollectionSync {
		return fmt.Errorf("%s's content block %q does not declare collection_sync, so gvm will "+
			"not rewrite its list from a collection", v.Manifest.Metadata.ID, v.Schema.Block)
	}
	if v.Schema.Collection == "" {
		return fmt.Errorf("%s declares no collection for block %q; add `collection: <id>` under "+
			"content.%s in %s", v.Name, v.Schema.Block, v.Schema.Block, v.Paths.Spec)
	}
	if mode := v.Schema.CollectionMode; mode == "off" {
		return fmt.Errorf("%s sets collection_sync: off for block %q, which is an instruction "+
			"not to do this", v.Name, v.Schema.Block)
	}

	w := env.Stdout
	if *noFetch {
		// A first-class mode, not an error path: it is the answer on a box with
		// no egress, and it prints what a sync WOULD be asked to reconcile
		// without pretending to know the collection's contents.
		fmt.Fprintf(w, "\n  collection %s, block %s\n", v.Schema.Collection, v.Schema.Block)
		fmt.Fprintf(w, "  --no-fetch: no request was made, so the collection's contents are "+
			"unknown here.\n  %d item(s) are declared locally:\n", len(v.Refs))
		for i, ref := range v.Refs {
			fmt.Fprintf(w, "    %2d  %s  %s\n", i, content.RefID(ref), ref.Note)
		}
		return nil
	}

	api, err := v.Host.workshopAPI(steamworkshop.Config{
		WorkshopAppID: v.Block.WorkshopAppID,
	})
	if err != nil {
		return err
	}

	col, source, err := fetchCollection(ctx, api, v.Schema.Collection, *allowHTML)
	if err != nil {
		return err
	}

	res, err := steamworkshop.Merge(v.Refs, col)
	if err != nil {
		return err
	}
	printCollectionMerge(w, v, col, source, res)

	if !res.Changed() {
		fmt.Fprint(w, "\n  the declared list already matches the collection; nothing was "+
			"written.\n")
		return nil
	}
	if *dry {
		fmt.Fprint(w, "\n  nothing has been written (--dry-run).\n")
		return nil
	}
	// The dated head comment above the appended block is section 12.5's, and
	// steamworkshop.Merge's doc comment names MergeResult.Added as what it is
	// rendered from: a YAML comment needs the YAML node, so it is the writer's
	// job and not the merge's.
	added := make([]content.AcquireID, 0, len(res.Added))
	for _, it := range res.Added {
		added = append(added, it.ID)
	}
	header := datedHeadComment(col.ID, v.Host.now().UTC().Format("2006-01-02"), added)
	if err := v.writeInstance(ctx, env, res.Refs, added, header); err != nil {
		return err
	}
	printOrderFindings(w, v.Name, content.CheckOrderOf(orderItemsOf(res.Refs)))
	afterListEdit(w, v, res.Refs)
	return nil
}

// fetchCollection runs the two paths in section 12.5's order.
//
// result==9 on the collection is "private or removed", and it is an ERROR
// rather than a fall-through to an empty list: an empty list is a perfectly
// valid thing for a collection to be, which is exactly why it cannot be the
// error value. The HTML fallback is reached only when the operator asked for
// it, and it is loud about having been used.
func fetchCollection(ctx context.Context, api *steamworkshop.API, id string, allowHTML bool) (
	*steamworkshop.Collection, string, error) {

	col, err := api.Collection(ctx, id)
	if err == nil {
		return col, "api", nil
	}
	if !errors.Is(err, steamworkshop.ErrCollectionUnavailable) || !allowHTML {
		return nil, "", err
	}
	col, herr := api.CollectionHTML(ctx, id)
	if herr != nil {
		// Both failures, because the first one is the diagnosis and the second
		// one is what happened when the operator tried the workaround.
		return nil, "", fmt.Errorf("%w; the HTML fallback also failed: %w", err, herr)
	}
	if eerr := api.Enrich(ctx, col); eerr != nil {
		return nil, "", fmt.Errorf("the collection page parsed but its items' metadata could "+
			"not be fetched, so the per-child cross-checks could not run: %w", eerr)
	}
	return col, "html", nil
}

func printCollectionMerge(w io.Writer, v *contentView, col *steamworkshop.Collection,
	source string, res *steamworkshop.MergeResult) {

	fmt.Fprintf(w, "\n  collection %s   %s\n", v.Schema.Collection, col.Summary())
	if source == "html" {
		fmt.Fprint(w, "  ! the API reported this collection as private or removed, so the "+
			"collection PAGE was scraped instead (--allow-html-fallback).\n"+
			"    The raw-id count and the paired count agreed, which is the only reason a "+
			"rewrite is being offered at all.\n")
	}
	fmt.Fprintln(w)

	for _, it := range res.Added {
		// Appended, never inserted: a new item's correct load position is
		// unknowable from a collection, and if it turns out to be a library it
		// has to be moved up by hand.
		fmt.Fprintf(w, "  + %s  %s\n", it.ID, it.Title)
	}
	for _, id := range res.Dropped {
		fmt.Fprintf(w, "  - %s  no longer in the collection\n", id)
	}
	for _, id := range res.Kept {
		// A vendored ref is operator bytes: never fetched, never pruned, and a
		// collection has no opinion about it.
		fmt.Fprintf(w, "  = %s  vendored, kept regardless of the collection\n", id)
	}
	for _, id := range col.Flagged() {
		it, _ := col.Item(id)
		// Recorded and KEPT. gvm does not delete a mod the operator asked for
		// on the strength of an API answer.
		fmt.Fprintf(w, "  ! %s  %s — kept; remove it yourself if that is what you want\n",
			id, flaggedReason(it))
	}
	if len(res.Added) == 0 && len(res.Dropped) == 0 {
		fmt.Fprintf(w, "  = all %s already declared, in the order this list has them\n",
			plural(len(col.Items), "item"))
	}
	if len(res.Added) > 0 {
		fmt.Fprintf(w, "\n  the %s above %s appended, because a collection cannot know that a\n"+
			"  library has to load before the mods that need it. `gvm content move %s <id> "+
			"--first`\n  is how one gets promoted.\n", plural(len(res.Added), "new item"),
			wasWere(len(res.Added)), v.Name)
	}
}

func flaggedReason(it steamworkshop.CollectionItem) string {
	switch {
	case it.Banned:
		if it.Upstream != nil {
			if r := strings.TrimSpace(it.Upstream.BanReason); r != "" {
				return "banned on the Workshop (" + r + ")"
			}
		}
		return "banned on the Workshop"
	case it.Removed:
		return "the Workshop reports it as private, removed, or never published"
	default:
		return "flagged by the Workshop"
	}
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
