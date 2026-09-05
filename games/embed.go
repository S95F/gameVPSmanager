// Package games holds the manifests gvm ships, compiled into the binary.
//
// This is the FLOOR of the three-layer resolution order (section 8.7):
// /etc/gvm/games shadows /usr/share/gvm/games shadows this. It is embedded
// rather than only packaged so that `gvm doctor` and `gvm adopt discover` still
// work on a box whose /usr/share was emptied by a bad upgrade — the one moment
// an operator most needs a tool that can still tell them what a game is
// supposed to look like.
//
// The package deliberately depends on nothing. internal/spec's tests import it
// to validate every shipped manifest, and a package with imports of its own
// would eventually make that a cycle.
package games

import "embed"

// FS is rooted at this directory, so its entries are "<id>.yaml" with no
// prefix. That is exactly the shape spec.EmbeddedSource wants, and the same
// shape os.DirFS gives for /usr/share/gvm/games, so the embedded floor and a
// real directory are interchangeable in the catalog.
//
//go:embed *.yaml
var FS embed.FS
