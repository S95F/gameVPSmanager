// Command genlegacy writes internal/archive/testdata/legacy-pz-backup.tar.gz:
// a fixture shaped exactly like `tar -czf ... -C $DATA_DIR Server
// Saves/Multiplayer/servertest` from the bash suite's backup.sh.
//
// It is deliberately NOT part of the archive package: the fixture has to be
// produced by something other than internal/archive/create.go, or the legacy
// test would only prove that the writer can read itself.
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"time"
)

type entry struct {
	name string
	dir  bool
	mode int64
	body string
}

func main() {
	out := os.Args[1]

	// GNU tar records whole-second mtimes in the base header; a fixed instant
	// keeps the fixture byte-identical across regenerations.
	base := time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)

	entries := []entry{
		{name: "Server/", dir: true, mode: 0o755},
		{name: "Server/servertest.ini", mode: 0o644, body: "PublicName=Steve's Server\nRCONPassword=REDACTED\nMods=\n"},
		{name: "Server/servertest_SandboxVars.lua", mode: 0o644, body: "SandboxVars = {\n\tZombies = 3,\n}\n"},
		{name: "Server/servertest_spawnregions.lua", mode: 0o644, body: "function SpawnRegions()\n\treturn {}\nend\n"},
		{name: "Saves/Multiplayer/servertest/", dir: true, mode: 0o755},
		{name: "Saves/Multiplayer/servertest/map_t.bin", mode: 0o644, body: "\x00\x01map chunk table\x00"},
		{name: "Saves/Multiplayer/servertest/map_meta.bin", mode: 0o644, body: "\x00meta\x00"},
		{name: "Saves/Multiplayer/servertest/players.db", mode: 0o644, body: "SQLite format 3\x00players"},
		{name: "Saves/Multiplayer/servertest/map_1_1.bin", mode: 0o644, body: "chunk 1,1"},
	}

	f, err := os.Create(out)
	check(err)
	defer f.Close()

	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)

	for i, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Mode:    e.mode,
			Uid:     1001,
			Gid:     1001,
			Uname:   "pzserver",
			Gname:   "pzserver",
			ModTime: base.Add(time.Duration(i) * time.Minute),
			// GNU, because that is what a Debian VPS's tar writes.
			Format: tar.FormatGNU,
		}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		check(tw.WriteHeader(hdr))
		if !e.dir {
			_, err := tw.Write([]byte(e.body))
			check(err)
		}
	}
	check(tw.Close())
	check(zw.Close())
	fmt.Println("wrote", out)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
