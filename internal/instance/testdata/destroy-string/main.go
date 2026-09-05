// This program is a fixture that MUST NOT COMPILE, and compilefail_test.go
// fails if it ever does.
//
// It is the executable form of the claim section 10.1 makes: "a plain string
// does not compile where a destructive operation needs a path". A prose
// assertion about the type system rots the moment somebody adds a convenience
// overload, a []string variant, or an exported field to Jailed. This one
// breaks the build instead.
//
// It carries no build tag on purpose. A //go:build ignore line would make the
// go tool report "build constraints exclude all Go files", which is a
// different failure that would pass the test for the wrong reason.
package main

import (
	"context"
	"log"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
)

func main() {
	ref := &archive.Ref{
		Path:       "/var/backups/gvm/pz-main/pre-rebuild-20260904-182211.tar.gz",
		Instance:   "pz-main",
		Class:      archive.ClassPreserve,
		Reason:     "pre-rebuild",
		CreatedAt:  time.Now(),
		VerifiedAt: time.Now(),
	}

	// The mistake: a path that looks fine, is absolute, was even read out of
	// the manifest -- and has been proven nothing. In a shell script this is
	// the line that deletes somebody's world. Here it does not build.
	worldPaths := []string{"/home/pzserver/Zomboid/Saves/Multiplayer/servertest"}

	if err := instance.Destroy(context.Background(), ref, worldPaths); err != nil {
		log.Fatal(err)
	}
}
