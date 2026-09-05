// This program MUST COMPILE. It is the control for the two fixtures beside it.
//
// Without it, a typo in an import path, a renamed package or a deleted symbol
// would make every fixture fail to build and the compile-fail tests would pass
// while proving nothing at all -- the same failure mode the platform package's
// deps_test guards against from the other side.
//
// It is never run. Everything it names is absolute and Linux-shaped, so
// running it on the development machine would fail on the first Resolve.
package main

import (
	"context"
	"log"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
)

func main() {
	jail, err := instance.NewJail(instance.JailConfig{
		Allow:     []string{"/home/pzserver/Zomboid"},
		Protected: []string{"/home/pzserver/Zomboid/Server", "/home/pzserver/Zomboid/db"},
	})
	if err != nil {
		log.Fatal(err)
	}

	paths, err := jail.ResolveAll([]string{"Saves/Multiplayer/servertest"})
	if err != nil {
		log.Fatal(err)
	}

	ref := &archive.Ref{
		Path:       "/var/backups/gvm/pz-main/pre-rebuild-20260904-182211.tar.gz",
		Instance:   "pz-main",
		Class:      archive.ClassPreserve,
		VerifiedAt: time.Now(),
	}
	if err := instance.Destroy(context.Background(), ref, paths); err != nil {
		log.Fatal(err)
	}
}
