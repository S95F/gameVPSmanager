// This program is a fixture that MUST NOT COMPILE, and compilefail_test.go
// fails if it ever does.
//
// The string-typed fixture next door proves Destroy will not take a raw path.
// This one proves the other half: that the proof cannot be forged. If Jailed
// ever grows an exported field, or a New/FromString constructor, this file
// starts compiling and the jail becomes a naming convention.
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
		VerifiedAt: time.Now(),
	}

	// The forgery: skip Jail.Resolve and simply assert the answer.
	forged := instance.Jailed{
		root: "/home/pzserver/Zomboid",
		abs:  "/home/pzserver/Zomboid/Saves/Multiplayer/servertest",
	}

	if err := instance.Destroy(context.Background(), ref, []instance.Jailed{forged}); err != nil {
		log.Fatal(err)
	}
}
