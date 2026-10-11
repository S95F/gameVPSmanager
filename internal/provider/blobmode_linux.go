//go:build linux

package provider

import "io/fs"

// BlobMode is the mode a published blob carries: 0444, owned by
// gvm-fetch:gvm-content (section 5).
//
// Read-only is not belt-and-braces. The store is shared by every instance on
// the box, so a loader that rewrites a jar in place -- ServerStarterJar's
// --installer-force rewrites server.jar -- would be editing bytes that another
// world is also linked to. 0444 turns that into a visible failure instead of a
// silent cross-instance edit (section 20, item 14).
const BlobMode fs.FileMode = 0o444
