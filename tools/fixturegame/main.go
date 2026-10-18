// Command fixturegame is the tiny stand-in game server the journey CI test
// (ci/journey.sh) runs end to end through gvm. It is NOT shipped: it exists so
// the journey can prove the create -> apply -> start -> ready -> stop -> backup
// path on a real systemd host without the (unbuilt) install engine and without
// downloading a real 10 GB game.
//
// Contract with ci/fixturegame.yaml:
//   - it prints the readiness marker "SERVER STARTED" once it is up, which the
//     manifest's single log_marker probe waits for;
//   - it writes a world file under <data>/worlds so `gvm backup` has something
//     to archive (backup.world_paths points there);
//   - it handles SIGINT (the manifest's shutdown step) by saving once more and
//     exiting 0, so the stop is clean rather than a degraded SIGKILL;
//   - it reads its console (stdin): a "save" line rewrites the world and prints
//     "world saved", mirroring how a real server's save verb behaves.
//
// It is deliberately stdlib-only and CGO-free so it builds on the Windows CI
// matrix too (where it never runs) and as a static linux/amd64 binary under
// gvm's seccomp/arch sandbox.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	data := flag.String("data", "", "the instance data directory; the world is written under <data>/worlds")
	port := flag.Int("port", 0, "the UDP game port (logged only; readiness is a log marker, not a port probe)")
	flag.Parse()

	if *data == "" {
		fmt.Fprintln(os.Stderr, "fixturegame: -data is required")
		os.Exit(2)
	}

	worldDir := filepath.Join(*data, "worlds")
	if err := os.MkdirAll(worldDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "fixturegame: cannot create %s: %v\n", worldDir, err)
		os.Exit(1)
	}
	if err := save(worldDir, "boot"); err != nil {
		fmt.Fprintf(os.Stderr, "fixturegame: initial save failed: %v\n", err)
		os.Exit(1)
	}

	// The readiness marker. stdout is line-buffered by gvm's console pipe, so a
	// trailing newline is enough; flush anyway in case anyone wraps us.
	fmt.Printf("listening on udp :%d\n", *port)
	fmt.Println("SERVER STARTED")
	os.Stdout.Sync()

	// A console reader, so a `save` verb has something to drive. Runs until
	// stdin closes.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if sc.Text() == "save" {
				if err := save(worldDir, "console"); err != nil {
					fmt.Fprintf(os.Stderr, "fixturegame: save failed: %v\n", err)
					continue
				}
				fmt.Println("world saved")
				os.Stdout.Sync()
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("shutting down; saving world")
	if err := save(worldDir, "shutdown"); err != nil {
		fmt.Fprintf(os.Stderr, "fixturegame: final save failed: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Sync()
	os.Exit(0)
}

// save writes a small, deterministic-shaped world file so each backup captures
// real bytes. The reason string records what triggered it.
func save(worldDir, reason string) error {
	body := "fixturegame world\nsaved: " + reason + "\nat: " + strconv.FormatInt(time.Now().Unix(), 10) + "\n"
	return os.WriteFile(filepath.Join(worldDir, "world.dat"), []byte(body), 0o640)
}
