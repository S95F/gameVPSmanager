# Research: Process supervision, console I/O, and RCON for a Go game-server manager on systemd Linux

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- Source RCON packet layout (all int32 LITTLE-endian, signed): Size int32 | ID int32 | Type int32 | Body null-terminated string | 1 pad null byte. Size counts ONLY the remainder (ID+Type+Body+2 nulls), never itself. gorcon's constants encode this exactly: PacketHeaderSize=8, PacketPaddingSize=2, MinPacketSize=10, MaxPacketSize=4106 (4096 body + 8 + 2).
- RCON packet type integers: SERVERDATA_AUTH=3 (C->S), SERVERDATA_AUTH_RESPONSE=2 (S->C), SERVERDATA_EXECCOMMAND=2 (C->S), SERVERDATA_RESPONSE_VALUE=0. Type 2 is overloaded; only direction disambiguates it.
- RCON auth handshake: C->S {id:N, type:3, body:password}. Server MAY first send an empty {id:N, type:0} junk packet (some servers omit it - tolerate both). Then {id:N, type:2} = success, or {id:-1, type:2} = failure. Servers usually close the TCP socket right after a failure.
- Valve multi-packet trick: responses fragment at 4096 body bytes with no end marker. Send an empty SERVERDATA_RESPONSE_VALUE (type 0) after the EXECCOMMAND; SRCDS answers strictly in receive order, mirrors it back empty, THEN emits a second malformed RESPONSE_VALUE whose body is the 4 bytes 0x0000 0001 0000 0000. The empty mirror is your terminator; the malformed packet must be consumed and discarded. This is the documented 'Known issue'.
- Minecraft deviates: C->S max payload 1446 (total 1460), S->C max payload 4096 (total 4110); vanilla REJECTS a client-sent SERVERDATA_RESPONSE_VALUE with 'Unknown request 0', so the Valve terminator trick does not work. Use the mctools technique instead: send a second junk EXECCOMMAND with a distinct id and read until its ack arrives.
- Minecraft RCON payload is NOT strict US-ASCII: servers emit section-sign colour codes (0xA7 / 167). Decode as ISO-8859-1, not ASCII. server.properties keys: enable-rcon=true, rcon.port (default 25575), rcon.password, broadcast-rcon-to-ops (default false).
- Project Zomboid RCON: RCONPort=27015, RCONPassword= in <servername>.ini. Useful commands: save, quit (saves then shuts down), players, servermsg "text", kickuser, banuser. RCON does not listen until the server is fully booted, so it is unusable during first boot / workshop download.
- Rust RCON quirks handled by gorcon: server emits type-4 packets that must be discarded, and answers 'Say' with id -1 which the library force-remaps to the request id. Rust also needs '+rcon.web 0' on the server command line to speak binary RCON instead of websocket RCON.
- systemd.kill(5) KillMode= values: control-group (default; KillSignal goes to ALL processes in the cgroup), mixed (KillSignal to the MAIN process only, SIGKILL later to all remaining), process, none. 'mixed' is the correct value for a runner-as-main-process design.
- systemd always sends SIGCONT immediately after KillSignal=. That is exactly why the existing zomboid.service's 'KillSignal=SIGCONT' works as a no-op terminate step, leaving TimeoutStopSec=180 + SendSIGKILL=yes as the only hard deadline.
- TimeoutStopSec= has two jobs: it bounds EACH ExecStop= command, and it bounds the wait for the service itself to stop. 'If any of them times out, subsequent ExecStop= commands are skipped and the service will be terminated by SIGTERM.' Value 'infinity' disables.
- ExecStop= only runs if the service started successfully, and systemd explicitly warns it is 'usually not sufficient to specify a command ... that only asks the service to terminate ... but does not wait for it to do so.' The current screen -X ExecStop chain is precisely that anti-pattern.
- Type=notify + sd_notify READY=1 gives real readiness. EXTEND_TIMEOUT_USEC=<usec> lets a service push TimeoutStartSec out repeatedly - essential for a PZ first boot / workshop download that took 2400-3600s in the bash suite. Type=notify-reload requires systemd 253 and is NOT available on Debian 12 (252) or Ubuntu 22.04 (249).
- sd_notify fields available: READY=1, RELOADING=1, MONOTONIC_USEC=, STOPPING=1, STATUS=<one-line UTF-8>, ERRNO=, MAINPID=, WATCHDOG=1, WATCHDOG=trigger, WATCHDOG_USEC=, EXTEND_TIMEOUT_USEC=, FDSTORE=1, FDNAME=, FDPOLL=0, FDSTOREREMOVE=1, BARRIER=1, NOTIFYACCESS=. Transport is AF_UNIX SOCK_DGRAM at $NOTIFY_SOCKET (path, or '@' for abstract), with SCM_CREDENTIALS. Gated by NotifyAccess= (default main).
- File descriptor store: FileDescriptorStoreMax=<n> (default 0 = disabled) plus sd_pid_notify_with_fds(FDSTORE=1, FDNAME=x). FDs come back on the next restart via the socket-activation protocol ($LISTEN_FDS, $LISTEN_FDNAMES, $LISTEN_PID, SD_LISTEN_FDS_START=3). FileDescriptorStorePreserve= is systemd 254+ - NOT on Debian 12 or Ubuntu 22.04.
- coreos/go-systemd/v22/daemon has NO file-descriptor passing: only SdNotify(unsetEnvironment bool, state string) (bool, error), SdWatchdogEnabled, SdNotifyMonotonicUsec, and the constants SdNotifyReady/Stopping/Reloading/Watchdog. FDSTORE must be hand-rolled with golang.org/x/sys/unix.Sendmsg + unix.UnixRights.
- coreos/go-systemd/v22/dbus is pure Go (no cgo). Key methods: NewSystemdConnectionContext (private socket, no dbus-daemon), StartUnitContext/StopUnitContext/RestartUnitContext/ReloadOrRestartUnitContext/KillUnitWithTarget/ResetFailedUnitContext, EnableUnitFilesContext/DisableUnitFilesContext/LinkUnitFilesContext/MaskUnitFilesContext, ReloadContext (daemon-reload), ListUnitsContext/ListUnitsByNamesContext, GetUnitPropertiesContext/SetUnitPropertiesContext, StartTransientUnitContext, Subscribe/SetSubStateSubscriber/SetPropertiesSubscriber. Job modes: replace, fail, isolate, ignore-dependencies, ignore-requirements.
- coreos/go-systemd/v22/sdjournal REQUIRES cgo and journal headers - it will not cross-compile from Windows. The pure-Go 'journal' package is WRITE-only. For reading, use `journalctl -o json --follow` as a subprocess, or systemd-journal-gatewayd on port 19531 (/entries?follow, /browse, /machine, /fields/<F>, /boots; Accept: text/plain | application/json | text/event-stream | application/vnd.fdo.journal).
- journald rate limit defaults to 10000 messages per 30s PER SERVICE and silently drops the rest (it logs a 'dropped' summary). Per-unit override exists: LogRateLimitIntervalSec=0 and LogRateLimitBurst=0 in the service unit. A chatty modded server will otherwise lose console lines.
- systemd.resource-control(5) knobs for per-instance limits: Slice=, MemoryAccounting=, MemoryMin/Low/High/Max/SwapMax/ZSwapMax (bytes or %, 'infinity'), CPUWeight= (1-10000 or 'idle', kernel default 100), CPUQuota= (%, can exceed 100%), CPUQuotaPeriodSec= (default 100ms, clamped 1ms-1000ms), AllowedCPUs=, TasksMax=, IOWeight= (1-10000), IOReadBandwidthMax=/IOWriteBandwidthMax= (device + bytes/s), IPAddressAllow/Deny, ManagedOOMSwap/ManagedOOMMemoryPressure (auto|kill), ManagedOOMMemoryPressureLimit= (0-100%), Delegate=.
- OOMPolicy= values: continue, stop, kill. Turning on MemoryAccounting for one unit implicitly turns it on for its whole slice and all parent slices. cgroup v2 unified hierarchy is the default on Debian 11+ and Ubuntu 21.10+, so all three targets (Debian 12 / Ubuntu 22.04 / 24.04) are cgroup v2.
- DynamicUser=yes allocates a UID/GID from 61184..65519 at start and releases it at stop; it implies NoNewPrivileges, RestrictSUIDSGID, RemoveIPC, ProtectSystem=strict, ProtectHome=read-only, and relocates StateDirectory/LogsDirectory/CacheDirectory under /var/lib/private, /var/log/private, /var/cache/private with symlinks.
- StateDirectory= -> /var/lib/<name> (persists), RuntimeDirectory= -> /run/<name> (innermost subdirs removed on stop), LogsDirectory= -> /var/log/<name>, CacheDirectory= -> /var/cache/<name>. Ownership is set from User=/Group= and recursively fixed if it does not match.
- Go syscall.SysProcAttr (Linux) fields relevant here: Setsid, Setpgid, Pgid, Setctty, Ctty (child's fd number), Noctty, Foreground, Pdeathsig, Credential (*syscall.Credential with Uid/Gid/Groups/NoSetGroups), AmbientCaps, NoNewPrivs, Cloneflags, UseCgroupFD + CgroupFD (CLONE_INTO_CGROUP), PidFD *int (CLONE_PIDFD). Setctty is only meaningful when Setsid is true.
- creack/pty v1.1.24 API: Start(cmd *exec.Cmd) (*os.File, error); StartWithSize(cmd *exec.Cmd, ws *Winsize) (*os.File, error); StartWithAttrs(c *exec.Cmd, sz *Winsize, attrs *syscall.SysProcAttr) (*os.File, error); Open() (pty, tty *os.File, err error); Getsize/GetsizeFull/Setsize/InheritSize; Winsize{Rows, Cols, X, Y uint16}; ErrUnsupported. Linux/darwin only - must be behind a //go:build linux tag for a Windows dev box.
- os/exec: when Stdin/Stdout/Stderr are *os.File the fd is passed straight through with no copier goroutine; any other io.Reader/Writer spawns a goroutine and Wait() blocks on it. StdinPipe/StdoutPipe/StderrPipe are closed by Wait, so Wait must not be called before all reads finish, and Run() cannot be used with them. WaitDelay bounds both a lingering process after context cancel and unclosed pipes.
- Project Zomboid's start-server.sh does NOT exec the JVM - it launches ./ProjectZomboid64 in a subshell, so the shell is MainPID and signals sent to it never reach the JVM. Any supervisor must signal the process group or walk the cgroup, not MainPID.
- Shutdown semantics differ per game and must be a per-game recipe, not one signal: Project Zomboid needs console/RCON 'save' then 'quit'; Minecraft's SIGTERM shutdown hook does save (but the reliable path is the 'stop' console/RCON command); Valheim ignores SIGTERM and needs SIGINT to save and exit cleanly (>= 0.143.5).
- Minecraft/JLine: 'JLine won't be able to access the tty if pipe is used to handle stdio, therefore it falls back to a dumb terminal.' A dumb terminal still reads commands from stdin fine. -Djline.terminal=jline.UnsupportedTerminal and --nogui are the standard flags for headless piped operation.
- systemd D-Bus properties for a dashboard, no /proc scraping needed: ActiveState, SubState, LoadState, Result, MainPID, ControlPID, ExecMainPID, ExecMainStartTimestamp, ExecMainStatus, NRestarts, MemoryCurrent, MemoryPeak, CPUUsageNSec, TasksCurrent, IOReadBytes, ControlGroup, StatusText. Signals JobRemoved(u id, o job, s unit, s result), UnitNew, PropertiesChanged require Subscribe() first.
- FIFO-as-stdin gotcha (the pzwiki-recommended pattern): when the last writer closes a FIFO the reader sees EOF and many game servers then exit or spin. The fix is for the holder to open the FIFO O_RDWR so a writer always exists. systemd's ExecStartPre= also cannot mkfifo a path that StandardInput= already needs to exist (systemd/systemd#10545).

## Pitfalls

- Default KillMode=control-group means `systemctl restart gvmd` SIGKILLs every game process in the daemon's cgroup. If the daemon forks games as children (Model B) this silently corrupts worlds on every daemon upgrade. KillMode=process/none 'allows processes to escape the service manager's lifecycle and resource management' - systemd's own warning - and also forfeits MemoryMax/CPUWeight on the games.
- ExecStop= commands that return immediately (screen -X stuff 'quit') do not wait for the game to exit. systemd then proceeds to the kill step. The current suite only survives this because KillSignal=SIGCONT neuters it - remove that line and worlds start corrupting. Any ExecStop must BLOCK until the game process is actually gone.
- SIGKILL during a PZ/Minecraft world save leaves half-written chunk/region files. The 180s TimeoutStopSec in the current unit is not generous: a large modded PZ world with 8 players can exceed it. Budget TimeoutStopSec>=300 and make the runner emit EXTEND_TIMEOUT_USEC while the save is demonstrably progressing.
- journald drops console lines above 10000 msgs/30s per service by default. A modded Minecraft or PZ server spewing mod warnings will have holes in the dashboard console and in crash post-mortems. Set LogRateLimitIntervalSec=0 / LogRateLimitBurst=0 per unit.
- sdjournal needs cgo + libsystemd headers and will break `GOOS=linux go build` from the Windows dev box. Do not reach for it; shell out to journalctl -o json or read the runner's own ring buffer.
- coreos/go-systemd/daemon cannot pass file descriptors, so the FDSTORE-based 'daemon restart keeps its pipes' design requires hand-written SCM_RIGHTS. Worse, FileDescriptorStorePreserve= is systemd 254+, absent on Debian 12 (252) and Ubuntu 22.04 (249), so on two of three targets the store survives a restart job but not a stop-then-start.
- Re-adopting an orphaned child after a daemon restart by PID alone is a PID-reuse race. You must pin identity with /proc/<pid>/stat field 22 (starttime, in clock ticks since boot) or a pidfd - and once reparented to PID 1 you can no longer wait() it, so you lose the exit status and must poll.
- Project Zomboid's start-server.sh runs the JVM in a subshell without exec. systemd's MainPID is the shell. Sending SIGTERM/SIGINT to MainPID does nothing to the game; process-group or cgroup enumeration is mandatory.
- Valheim ignores SIGTERM entirely and only saves on SIGINT; Minecraft saves on SIGTERM; PZ needs an in-band 'save'+'quit'. A single hardcoded stop signal will corrupt at least one of these. The stop path must be a declarative per-game recipe.
- PZ RCON does not bind until the server has fully booted, and during first boot / workshop download that can be 40-60 minutes. An RCON-only control plane has no way to observe or steer that phase. Console capture must be the primary channel and RCON the accelerator.
- The Valve terminator trick (client-sent empty SERVERDATA_RESPONSE_VALUE) throws 'Unknown request 0' on Minecraft and errors on several other non-Source games. Applying it unconditionally breaks the second-most-important target platform.
- Strict packet-ID matching breaks on Rust (answers 'Say' with id -1) and on games that mangle the echo. A hard `if resp.ID != req.ID { return err }` will randomly fail in production; make strictness a per-game profile flag.
- Treating a <4096-byte RCON body as end-of-response is a heuristic, not a guarantee - a response that is an exact multiple of 4096 or a slow fragment will truncate or bleed into the next command's buffer. Unread fragments poison every subsequent command on that connection.
- RCON is plaintext with no encryption and no rate limiting. Binding it to 0.0.0.0 exposes full server admin to anyone who can brute-force the password. Bind to 127.0.0.1 and reach it only from the local daemon; the firewall script must never open the RCON port.
- MemoryMax= on a JVM game server triggers the kernel OOM killer, not a graceful GC - the same failure the existing config.env warns about ('B42 uses significant native memory OUTSIDE the Java heap'). Use MemoryHigh= (throttle) as the working limit and MemoryMax= only as a far-above backstop, and set OOMPolicy=stop so a kill is reported as a unit failure instead of silently leaving a half-dead tree.
- MemoryAccounting=yes on one unit implicitly enables it for the entire slice and all parent slices - fine here, but it means per-instance accounting numbers change once you introduce gvm.slice, so don't build alerting on pre-slice baselines.
- DynamicUser=yes cannot be used for game instances: the UID changes across restarts while the world files on disk do not, ProtectSystem=strict blocks the SteamCMD install tree, and /var/lib/private relocation breaks the existing /home/pzserver/Zomboid layout and the migration path for the live server.
- A FIFO used as stdin gives the game EOF the moment the last writer closes it; many servers then busy-loop or exit. Only safe if the holder keeps an O_RDWR fd open forever. systemd also cannot mkfifo the path in ExecStartPre when StandardInput= already requires it to exist.
- StdinPipe/StdoutPipe from os/exec are closed by Wait(), so a naive `go io.Copy(ring, stdout); cmd.Wait()` races and truncates the last lines of a crash - exactly the lines you need. Read to EOF before Wait, or pass *os.File / a PTY master which bypasses the copier goroutine entirely.
- creack/pty is Linux/darwin only. Without a //go:build linux tag on every file that touches it (and on any file using syscall.SysProcAttr's Linux-only fields, unix.Sendmsg, pidfd, or /proc), the Windows dev build breaks.

## Recommendations

- Adopt Model C (hybrid). One templated unit per instance, gvm@<id>.service, whose ExecStart is `/usr/local/bin/gvm runner --instance %i`. The runner is a thin per-instance supervisor inside the same binary; the long-running manager daemon (gvmd) never owns a game process.
- Give the runner Type=notify. It emits READY=1 only when the game's per-profile 'started' marker appears (PZ: 'SERVER STARTED'; Minecraft: 'Done (' / 'For help, type'), STATUS= lines for the dashboard, and EXTEND_TIMEOUT_USEC during long first boots and workshop/CurseForge downloads. Set TimeoutStartSec=infinity only as a last resort - prefer EXTEND_TIMEOUT_USEC so a genuinely hung boot still fails.
- Set KillMode=mixed and DO NOT write an ExecStop=. On `systemctl stop`, systemd SIGTERMs only the runner; the runner executes the per-game stop recipe, waits for the real game PID to exit, then exits itself. This deletes the KillSignal=SIGCONT hack, the sleep 10, and the screen -X chain, and makes 'saved cleanly' vs 'got SIGKILLed' an observable outcome instead of a guess.
- Model the stop path as declarative data per game profile, not code: an ordered list of steps, each {via: rcon|stdin|signal, payload, wait_for: <log regex>|<duration>, timeout}. PZ = [stdin/rcon 'save' wait 15s, 'quit', wait exit 300s]. Minecraft = ['stop' via rcon or stdin, wait exit 300s]. Valheim = [signal SIGINT, wait exit 120s]. Fallback chain ends in SIGTERM then SIGKILL, both logged as a degraded shutdown.
- Have the runner allocate a PTY with creack/pty (StartWithSize) rather than plain pipes, and pass the PTY master as the child's stdin/stdout/stderr. It is an *os.File so os/exec passes the fd straight through with no copier goroutine, it makes JLine-style consoles behave, and it merges stdout/stderr in true interleaved order the way screen did. Keep a plain-pipes mode behind a per-profile flag for servers that misbehave on a TTY.
- Replace GNU screen with a Unix socket owned by the runner: RuntimeDirectory=gvm/%i gives /run/gvm/<id>/, and the runner listens on /run/gvm/<id>/console.sock (0660, group gvm). `gvm console <id>` dials it, sets raw mode with golang.org/x/term, and streams both ways; the web dashboard reaches the same socket through gvmd. Multiple readers, one shared ring buffer, no screen session to lose.
- Ring-buffer console output in the runner: a fixed-size byte ring (start at 1-2 MiB) plus a line index, exposed as {backlog, follow-from-cursor}. Also mirror raw output to StandardOutput=append:/var/log/gvm/%i/console.log via LogsDirectory=gvm/%i so history survives a runner restart, and set LogRateLimitIntervalSec=0 / LogRateLimitBurst=0 if you also send it to the journal.
- Put every instance under a gvm.slice with per-instance overrides: Slice=gvm.slice, MemoryAccounting=yes, MemoryHigh=<soft>, MemoryMax=<hard, well above the JVM heap+native headroom the config.env comments describe>, OOMPolicy=stop, CPUWeight=<100 default, lower for secondary instances>, CPUQuota=<optional>, TasksMax=, IOWeight=, LimitNOFILE=100000. Keep the existing 'leave ~3GB free above JVM_XMX' rule as a computed default rather than a hand-set number.
- Use one real unix user per instance (gvm-<id>, useradd -r), not DynamicUser and not a shared account. It preserves the existing /opt/<install> + /home/<user>/data layout, survives restarts so world files keep a stable owner, makes the live-PZ migration a chown-free no-op, and gives real filesystem isolation between instances. Add NoNewPrivileges=yes, PrivateTmp=yes, ProtectSystem=full, ProtectHome=yes with ReadWritePaths= for the instance's install and data dirs, and RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX.
- Talk to systemd over D-Bus with coreos/go-systemd/v22/dbus (pure Go, cross-compiles from Windows) using NewSystemdConnectionContext so it works without dbus-daemon. Use StartUnitContext/StopUnitContext/RestartUnitContext with mode 'replace', ReloadContext after writing unit files, and Subscribe + SetSubStateSubscriber for live state. Shell out to systemctl only in the installer/first-run path where a human is watching.
- Poll MemoryCurrent, MemoryPeak, CPUUsageNSec, TasksCurrent, NRestarts, ActiveState/SubState via GetUnitPropertiesContext for the dashboard's resource graphs. It is one D-Bus round trip per instance and needs no /proc parsing or cgroup path handling.
- Write ONE RCON client with a per-game capability profile rather than a per-game client. Profile fields: terminator_strategy (valve_empty_response | second_command | length_heuristic | none), strict_id (bool), charset (utf8 | latin1), max_command_len, discard_packet_types ([]int32 for Rust's type 4), auth_expects_junk_packet (tri-state: yes/no/tolerate). Default to tolerate/none and opt games in.
- Implement the RCON reader as: read exactly 4 bytes -> Size (int32 LE) -> sanity-check 10 <= Size <= 4096+10 -> read Size bytes -> parse ID, Type, Body up to first NUL, then the pad. Reject and resync on out-of-range sizes rather than trusting the wire. Cap total reassembled response length and always drain a poisoned connection by closing and redialing, never by reusing it.
- For Minecraft, use the second-command terminator: send EXECCOMMAND id=N, then EXECCOMMAND id=N+1 with a harmless command (a bare 'seed' or an empty body), and treat the response with id=N+1 as end-of-N. Never send a client-side SERVERDATA_RESPONSE_VALUE to Minecraft. Decode bodies as ISO-8859-1 and strip 0xA7-prefixed colour codes for the dashboard while keeping the raw bytes in the ring buffer.
- Treat RCON as an optional accelerator, never the control plane. Console (PTY + ring buffer) is the universal path and is available before RCON binds, during first boot, and for console-only games; RCON is used for structured queries (players, seed, tps) and as a second stop channel. Bind RCON to 127.0.0.1 in generated configs and never open its port in the firewall step.
- Keep the daemon stateless with respect to game processes so `systemctl restart gvmd` is a non-event. gvmd holds PostgreSQL state, unit templates, the web/Caddy front end, and short-lived connections to /run/gvm/<id>/console.sock. On start it re-enumerates instances via ListUnitsByNamesContext and reconnects to whichever console sockets are live.
- For the live PZ migration: archive first (tar the world exactly as backup.sh does), write the gvm instance row + gvm@pz.service from the parsed config.env and <servername>.ini WITHOUT touching the running server, then do one supervised cutover - screen -X stuff 'save', wait, stuff 'quit', wait for the JVM to actually exit, systemctl disable --now zomboid.service, systemctl enable --now gvm@pz.service. Adoption never rewrites Saves/ and never runs SteamCMD validate on the first pass.
- Guard every Linux-only surface with //go:build linux: creack/pty, syscall.SysProcAttr's Setsid/Setpgid/Credential/Pdeathsig, golang.org/x/sys/unix, /proc reads, and the D-Bus supervisor. Provide a stub implementation behind //go:build !linux so `go build`, `go vet` and the unit tests stay green on the Windows dev box, and gate the real integration tests on a Linux CI runner or a VPS.

## Detail

## 0. What the bash suite already encodes (and must survive)

Read from `C:/projectZomboid`. Four pieces of hard-won knowledge, and the exact mechanism each used:

| Knowledge | Current mechanism | Where it must land in gvm |
|---|---|---|
| Never SIGKILL a live world | `KillSignal=SIGCONT` + `SendSIGKILL=yes` + `TimeoutStopSec=180`, with `ExecStop` stuffing `save\015` then `quit\015` into screen | Runner-owned stop recipe that **blocks** until the game PID exits |
| Attachable console | `ExecStart=/usr/bin/screen -DmS pzserver ...`, attach via `sudo -u pzserver script -q /dev/null -c 'screen -r pzserver'` | Runner-owned PTY + `/run/gvm/<id>/console.sock` |
| Boot-marker gating | `pz_boot_and_wait "SERVER STARTED" 2400` polling `server-console.txt` | `Type=notify` + `READY=1` on marker + `EXTEND_TIMEOUT_USEC` |
| Workshop IDs ≠ Mod IDs | `sync-mods.sh` resolves numeric workshop dirs → internal `id=` from `mod.info`, writes both `WorkshopItems=` and `Mods=` | Out of scope here, but note it is a *two-list* model that generalises to CurseForge projectID vs modId |

Two latent bugs in the current unit that gvm must not inherit:

1. `ExecStop=/usr/bin/screen -p 0 -S pzserver -X eval "stuff 'quit\015'"` returns **immediately**. systemd's own man page: *"it is usually not sufficient to specify a command for this setting that only asks the service to terminate ... but does not wait for it to do so."* The only reason worlds survive today is that `KillSignal=SIGCONT` makes the subsequent terminate step a no-op (systemd always sends `SIGCONT` right after `KillSignal=`, so this sends `SIGCONT, SIGCONT`). It is correct, but it is load-bearing magic with no observability: you cannot tell "saved and exited at 40s" from "hung and got SIGKILLed at 180s".
2. `start-server.sh` launches `./ProjectZomboid64` in a **subshell without `exec`** ([reported upstream](https://steamcommunity.com/app/108600/discussions/6/3812906855248680121/)). systemd's `MainPID` is the shell; signals to it never reach the JVM. Anything that trusts `MainPID` for signalling is broken on PZ specifically.

---

## 1. Model comparison

### Model A — daemon generates one unit per instance, shells out to `systemctl`

**Attachable console.** systemd has no native interactive stdin. Three options, all bad:

- *Keep screen/tmux inside `ExecStart`* — status quo. You are now parsing `screen -X` exit codes (which lie: `screen -X stuff` returns 0 whether or not the session exists in some versions), you cannot stream output except by tailing `server-console.txt`, and a screen session that dies takes your only control channel with it.
- *`StandardInput=file:/run/gvm/<id>/stdin.fifo`* — the pattern pzwiki recommends. Real problems: systemd opens the FIFO at unit start, so `ExecStartPre=mkfifo` cannot create it ([systemd#10545](https://github.com/systemd/systemd/issues/10545)); and when the last writer closes, the game reads EOF on stdin and typically exits or busy-loops. The only safe holder opens it `O_RDWR` so a writer always exists. That holder has to be gvmd — which reintroduces gvmd as a single point of failure for stdin.
- *`StandardInput=socket` with a `.socket` unit* — inetd-style, single-connection semantics, awkward for a multiplexed console.

**Sending an arbitrary command.** Write to the FIFO, or `screen -X eval "stuff 'cmd\015'"`, or RCON. No delivery confirmation on any of them — Pterodactyl's Docker path has the same gap (`e.stream.Conn.Write([]byte(c + "\n"))`, no ack).

**Surviving daemon restart.** Perfect. Games are systemd's children; gvmd is irrelevant to them.

**Ring-buffering stdout.** `StandardOutput=journal` then either `journalctl -u gvm@<id> -o json --follow -n 500` as a subprocess (pure Go, cross-compiles, ~1 process per followed instance), `sdjournal` (**cgo — will not cross-compile from Windows**), or `systemd-journal-gatewayd` on **port 19531** (`/entries?follow`, `Accept: text/event-stream`). Watch the journald rate limit: **10000 msgs / 30s per service**, silently dropped; override per unit with `LogRateLimitIntervalSec=0` / `LogRateLimitBurst=0`. Alternative: `StandardOutput=append:/var/log/gvm/%i/console.log` and tail it yourself — no rate limit, but you own rotation.

**Verdict.** Strongest isolation/limits/restart story, weakest console story. The console is the deal-breaker: it is the one thing the old suite did well and the one thing this model makes worse.

### Model B — daemon is the supervisor, forks/execs each game

**Attachable console.** Best possible. `pty.StartWithSize(cmd, &pty.Winsize{Rows:40, Cols:120})` returns the master `*os.File`; because it is an `*os.File`, `os/exec` hands the fd straight to the child with no copier goroutine. In-memory ring buffer, WebSocket straight out, per-viewer cursors, zero subprocesses.

**Surviving daemon restart.** This is where the model dies.

- Default `KillMode=control-group` means `systemctl restart gvmd` sends `KillSignal` to **every process in gvmd's cgroup** — every game, mid-save.
- Escaping it needs `KillMode=process` or `none`, which systemd explicitly warns *"allows processes to escape the service manager's lifecycle and resource management, and to remain running even while their service is considered stopped"* — and forfeits `MemoryMax=`/`CPUWeight=` on the games, since they are no longer in a unit you control. The alternative is `StartTransientUnit` to drop each game into its own **scope**, which restores cgroup control but means you have re-implemented Model A's unit management anyway, badly.
- Re-adoption is a PID-reuse race. You must pin identity with `/proc/<pid>/stat` **field 22 (`starttime`, clock ticks since boot)** or a pidfd (`pidfd_open`, `pidfd_send_signal`). Once reparented to PID 1 you cannot `wait()` — no exit status, only polling.
- **Console FDs cannot be recovered from a dead parent.** The only mechanism is systemd's fd store: `FileDescriptorStoreMax=<n>` plus `sd_pid_notify_with_fds(FDSTORE=1, FDNAME=console-<id>)`, coming back on restart via `$LISTEN_FDS` / `$LISTEN_FDNAMES` / `SD_LISTEN_FDS_START=3`. Two blockers: `coreos/go-systemd/v22/daemon` has **no fd-passing** (only `SdNotify(bool, string)`), so you hand-roll `unix.Sendmsg` + `unix.UnixRights`; and `FileDescriptorStorePreserve=` is **systemd 254+**, absent on Debian 12 (252) and Ubuntu 22.04 (249), so on two of three targets the store survives a *restart job* but not a *stop-then-start*.

**Verdict.** Best console, worst operations. Rejected.

### Model C — hybrid: unit per instance, `ExecStart` is the daemon in runner mode

```
ExecStart=/usr/local/bin/gvm runner --instance %i
```

The runner is the **main process of its own unit**. It owns the PTY, the ring buffer, the console socket, and the stop recipe. gvmd owns Postgres, the web tier, Caddy config, and nothing else.

- **Console** — identical to Model B: the runner has the PTY master in-process.
- **Arbitrary command** — write to the PTY master (or the game's stdin pipe), or the runner's RCON connection. The runner can echo the command into its own ring buffer so the dashboard shows what was sent, and can correlate the game's echo back.
- **Daemon restart** — gvmd holds zero game processes. `systemctl restart gvmd` is a non-event. On start it calls `ListUnitsByNamesContext` and reconnects to `/run/gvm/<id>/console.sock`.
- **Runner restart** — does not happen independently; the runner *is* the unit. No fd juggling, no FDSTORE, no PID-reuse race, no re-adoption code at all.
- **Limits** — full systemd cgroup control, because the game is inside a real unit's cgroup.
- **Shutdown** — `KillMode=mixed` sends `KillSignal` to the **main process only** (the runner) and `SIGKILL` to everything remaining at timeout. The runner catches SIGTERM, runs the recipe, waits for the real game PID, exits. **No `ExecStop=` at all.**

**Verdict.** Recommended.

---

## 2. The unit template

```ini
# /etc/systemd/system/gvm@.service
[Unit]
Description=gvm game server instance %i
After=network-online.target
Wants=network-online.target
PartOf=gvm.target

[Service]
Type=notify
NotifyAccess=main
User=gvm-%i
Group=gvm-%i
Slice=gvm.slice
WorkingDirectory=/opt/gvm/instances/%i
RuntimeDirectory=gvm/%i
RuntimeDirectoryMode=0750
StateDirectory=gvm/%i
LogsDirectory=gvm/%i

ExecStart=/usr/local/bin/gvm runner --instance %i

# The runner is the main process; only IT gets SIGTERM.
KillMode=mixed
KillSignal=SIGTERM
TimeoutStartSec=infinity          # runner drives readiness via EXTEND_TIMEOUT_USEC
TimeoutStopSec=300                # runner extends this while a save is progressing
SendSIGKILL=yes
Restart=on-failure
RestartSec=20

# Console: runner owns the PTY. Journal only carries runner diagnostics.
StandardOutput=journal
StandardError=journal
LogRateLimitIntervalSec=0
LogRateLimitBurst=0

# Resource control (per-instance drop-in overrides these)
MemoryAccounting=yes
MemoryHigh=6G
MemoryMax=7G
OOMPolicy=stop
CPUWeight=100
TasksMax=4096
LimitNOFILE=100000

# Hardening
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=yes
ReadWritePaths=/opt/gvm/instances/%i /var/lib/gvm/%i /var/log/gvm/%i
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

Per-instance tuning goes in `/etc/systemd/system/gvm@<id>.service.d/50-limits.conf` so the template stays generic and gvm rewrites only the drop-in.

`Type=notify` works on every target (`notify-reload` needs systemd 253 — **not available** on Debian 12 or Ubuntu 22.04, do not use it).

---

## 3. Console I/O design

**PTY vs pipes.** Prefer a PTY (`creack/pty` v1.1.24, `StartWithSize`). Reasons: `*os.File` bypasses `os/exec`'s copier goroutine entirely; stdout and stderr interleave in real order (what screen gave you); JLine-based consoles get a terminal instead of falling back to dumb mode (*"JLine won't be able to access the tty if pipe is used to handle stdio, therefore it falls back to a dumb terminal"*). Downsides: local echo of typed commands, and `\r\n` line endings — normalise both in the ring buffer. Keep a `pipes` mode as a per-profile escape hatch.

**Attach surface.** `RuntimeDirectory=gvm/%i` gives `/run/gvm/<id>/`. Runner listens on `/run/gvm/<id>/console.sock`, mode 0660, group `gvm`. `gvm console <id>` dials it and calls `term.MakeRaw(int(os.Stdin.Fd()))`; the dashboard reaches the same socket through gvmd, which relays to a WebSocket. Detach is just closing the socket — no session state to lose, unlike screen.

**Ring buffer.** Fixed-size byte ring (start 1–2 MiB) + a line-offset index + a monotonic cursor. Protocol on the socket: client sends `{backlog: N}` or `{from_cursor: C}`, gets a snapshot then a live tail. Simultaneously mirror raw bytes to `/var/log/gvm/<id>/console.log` (`LogsDirectory=`) so history survives a runner restart and post-mortems have the crash tail — the tail that a naive `go io.Copy(...); cmd.Wait()` would truncate, since `Wait` closes `StdoutPipe`.

**Command injection.** Two channels, chosen per profile: PTY write (universal, works before RCON binds) and RCON (structured, gets a response body). Log every injected command with actor identity into Postgres — that is where the RBAC requirement actually bites.

---

## 4. Clean shutdown

`ExecStop=` is deleted. The runner implements a declarative recipe:

```go
type StopStep struct {
    Via      string        // "rcon" | "stdin" | "signal"
    Payload  string        // "save" | "quit" | "stop" | "SIGINT"
    WaitFor  string        // log regex, optional
    Wait     time.Duration
}
```

Per-game, from research:

- **Project Zomboid** — `save`, wait ~15s (or for the save-complete log line), `quit`, wait for exit up to 300s. Works over stdin or RCON (`RCONPort=27015`).
- **Minecraft** — `stop` via RCON or stdin. SIGTERM also triggers a saving shutdown hook, but `stop` is the deterministic path and works identically for Forge/NeoForge/Fabric.
- **Valheim** — **`SIGINT`, not SIGTERM.** Valheim ignores SIGTERM and only saves on SIGINT (≥ 0.143.5).

Terminal fallback ladder, always logged as a degraded shutdown: recipe → SIGTERM to the game's process group → SIGKILL. Because PZ's `start-server.sh` does not `exec`, signal the **process group** (`Setpgid: true` at spawn, then `unix.Kill(-pgid, sig)`) or enumerate `cgroup.procs`, never `MainPID`.

While the game is still writing (region files changing mtime, or a save-progress log line matching), the runner emits `EXTEND_TIMEOUT_USEC=` so a genuinely slow save is not guillotined at `TimeoutStopSec`, while a hung one still eventually fails.

Why SIGKILL corrupts: both PZ and Minecraft write world state as many small region/chunk files. A kill mid-write leaves a truncated or half-rewritten file that the loader either rejects or silently reads as empty terrain. The suite's own `config.env` warns about the adjacent failure — the kernel OOM-killing the JVM during world generation — which is the same corruption arriving from a different direction, and the reason `MemoryMax=` must not be set at the JVM heap size.

---

## 5. Resource limits and user isolation

**Slices.** `gvm.slice` under `system.slice`; every instance `Slice=gvm.slice`. Cgroup path `/sys/fs/cgroup/system.slice/gvm.slice/gvm@<id>.service`. cgroup v2 unified is default on Debian 11+ and Ubuntu 21.10+, so all three targets are fine.

**Memory.** `MemoryHigh=` (throttle + reclaim pressure) is the working limit; `MemoryMax=` is a far-above backstop, because exceeding `MemoryMax` invokes the kernel OOM killer, not a graceful GC. `OOMPolicy=stop` makes a kill surface as a unit failure rather than a half-dead tree. Note that `MemoryAccounting=yes` on one unit implicitly enables it for the whole slice and every parent slice.

**CPU.** `CPUWeight=` 1–10000 (kernel default 100) is a *share*, correct for "the PZ server should win against the Minecraft server under contention". `CPUQuota=` is a hard cap (can exceed 100% for multi-core) — use only for tenant-style hard limits. `CPUQuotaPeriodSec=` defaults to 100ms, clamped 1ms–1000ms.

**Users.** One real system user per instance (`gvm-<id>`, `useradd -r`). Not `DynamicUser=` — its UIDs come from 61184..65519 and change across restarts while world files do not, and it implies `ProtectSystem=strict` + `/var/lib/private` relocation, which breaks both the SteamCMD install tree and the existing `/home/pzserver/Zomboid` layout the live-server migration depends on. Not shared either: shared means one compromised mod reads every world.

---

## 6. Go specifics

| Need | Choice | Note |
|---|---|---|
| systemd control | `coreos/go-systemd/v22/dbus` | **Pure Go**, cross-compiles. `NewSystemdConnectionContext` uses systemd's private socket — no dbus-daemon needed. |
| readiness/status | `coreos/go-systemd/v22/daemon` `SdNotify` | No fd-passing; fine, Model C does not need it. |
| journal read | `journalctl -o json --follow` subprocess, or gatewayd :19531 | **Never `sdjournal`** — cgo, breaks the Windows build. |
| PTY | `creack/pty` v1.1.24 | Linux/darwin only → `//go:build linux`. |
| child attrs | `syscall.SysProcAttr{Setsid, Setpgid, Pgid, Setctty, Ctty, Credential, Pdeathsig, NoNewPrivs}` | `Setctty` only meaningful with `Setsid: true`; `Ctty` is the **child's** fd number. |
| socket activation | `coreos/go-systemd/v22/activation` | Not needed for Model C; keep in mind for a future gvmd zero-downtime reload of the HTTP listener behind Caddy. |

Dashboard metrics via `GetUnitPropertiesContext`: `ActiveState`, `SubState`, `MainPID`, `ExecMainStartTimestamp`, `ExecMainStatus`, `NRestarts`, `MemoryCurrent`, `MemoryPeak`, `CPUUsageNSec`, `TasksCurrent`, `ControlGroup`, `StatusText`. Live state via `Subscribe()` + `SetSubStateSubscriber` and the `JobRemoved(u id, o job, s unit, s result)` signal (`result` ∈ done/canceled/timeout/failed/dependency/skipped).

Build hygiene: put every Linux-only surface behind `//go:build linux` with a `//go:build !linux` stub, so `go build`/`go vet`/unit tests stay green on Windows 11.

---

## 7. SOURCE RCON — exact protocol

**Packet (all int32 little-endian, signed):**

```
offset  size  field
0       4     Size   int32   bytes in the REMAINDER (id+type+body+2 nulls); does NOT include itself
4       4     ID     int32   client-chosen; echoed by server
8       4     Type   int32
12      n     Body   ASCII/UTF-8, NUL-terminated
12+n    1     Pad    0x00    (the "empty string" second terminator)
```

Corroborated by `gorcon`'s constants: `PacketHeaderSize=8`, `PacketPaddingSize=2`, `MinPacketSize=10`, `MaxPacketSize=4106` (= 4096 body + 8 + 2). Minimum wire bytes = 14 (4-byte Size field carrying the value 10).

**Types** — note the collision on 2, disambiguated only by direction:

| Const | Value | Direction |
|---|---|---|
| `SERVERDATA_AUTH` | 3 | C→S |
| `SERVERDATA_AUTH_RESPONSE` | 2 | S→C |
| `SERVERDATA_EXECCOMMAND` | 2 | C→S |
| `SERVERDATA_RESPONSE_VALUE` | 0 | S→C (and C→S for the terminator trick) |

**Auth handshake:**

1. C→S `{Size, ID: N, Type: 3, Body: password}`
2. S→C `{ID: N, Type: 0, Body: ""}` — an empty junk `SERVERDATA_RESPONSE_VALUE`. **Some servers omit it.** Tolerate both; gorcon explicitly detects and skips it.
3. S→C `{ID: N, Type: 2}` = success (ID mirrored) · `{ID: -1, Type: 2}` = failure. Servers generally close the TCP socket immediately after a failure — a closed connection right after auth *is* the error signal.

**Multi-packet responses.** S→C fragments at 4096 body bytes. There is no length prefix for the logical response and no end marker.

*Valve terminator trick:* immediately after `EXECCOMMAND` id=N, send an **empty `SERVERDATA_RESPONSE_VALUE` (type 0)**. SRCDS answers requests strictly in receive order, so once it mirrors that empty packet back, every real fragment has already arrived.

*The documented Known Issue:* the server mirrors your erroneous type-0 packet back empty, **and then sends a second `SERVERDATA_RESPONSE_VALUE` whose body is the 4 bytes `0x0000 0001 0000 0000`**. That trailing malformed packet must be read and discarded or it poisons the next command's response.

*ID mismatch quirks:* Rust answers `Say` with `ID = -1` (gorcon force-remaps it to the request id) and emits stray **type-4** packets that must be discarded. A strict `resp.ID != req.ID → error` will randomly fail across the game matrix; make strictness a per-profile flag.

*Fallback heuristic:* "body < 4096 means end" is **not reliable** — a response that is an exact multiple of 4096, or a slow fragment, truncates or bleeds.

Other limits: gorcon caps `MaxCommandLen` at 1000 and uses 5s dial + 5s deadline by default.

---

## 8. Minecraft's deviations

- Same type integers (3 login, 2 command, 0 response); auth failure returns **ID = -1**; successful auth comes back with **Type 2** and matching ID.
- **C→S max payload 1446 (total 1460); S→C max payload 4096 (total 4110).**
- **The Valve terminator trick fails.** Vanilla rejects a client-sent `SERVERDATA_RESPONSE_VALUE` with **"Unknown request 0"**. Use the mctools approach instead: send a second junk **command** packet with a distinct ID and read until the server acks it — safe because *"all packets are sent in the order that they are received"*.
- **Charset:** not strict US-ASCII. Servers emit section-sign (`0xA7` / 167) colour codes. Decode **ISO-8859-1**; the Minecraft wiki calls out US-ASCII as wrong here.
- `server.properties`: `enable-rcon=true`, `rcon.port` (default **25575**), `rcon.password`, `broadcast-rcon-to-ops` (default `false`). The wiki's own warning: *"It is dangerous to expose RCON ports to the internet. RCON is not encrypted."*
- Modded (Forge/NeoForge/Fabric) inherits the vanilla RCON handler unchanged, including its single-threaded blocking behaviour — a slow command stalls the RCON thread.

**Project Zomboid**, by contrast, is close to stock Source: `RCONPort=27015`, `RCONPassword=` in `<servername>.ini`; commands `save`, `quit`, `players`, `servermsg "text"`, `kickuser`, `banuser`. Critically, **RCON does not bind until the server is fully booted** — during the 40–60 minute first boot and workshop download the bash suite grep'd `server-console.txt` for `SERVER STARTED` precisely because nothing else was reachable.

---

## 9. Recommendation

**Model C, with the runner as the per-instance supervisor.**

The decisive argument is not the console (B wins that) or the restart story (A wins that) — it is that C wins both, at the cost of one extra process per instance (~5 MB RSS for a Go runner). Concretely:

1. **Console-first, RCON-second** is forced by the target order. Steam-via-SteamCMD games are heterogeneous: PZ has RCON but not during boot; Valheim has no RCON and needs SIGINT; console-only titles exist throughout. A manager that must support both RCON games and console-only games cannot make RCON the control plane. The runner's PTY is the one channel that always exists, from the first byte of first boot to the last byte of shutdown.
2. **RCON becomes a capability, not an architecture.** One client, a per-game profile (`terminator_strategy`, `strict_id`, `charset`, `max_command_len`, `discard_packet_types`, `auth_expects_junk_packet`). Adding a game is a config row, not a code path.
3. **The SIGCONT hack disappears** and becomes an observable, testable Go function that waits for the actual PID and reports which rung of the fallback ladder fired.
4. **`systemctl restart gvmd` stops being scary**, which matters a great deal when a live PZ world with real players is on the box and you are shipping dashboard changes.
5. **The migration path is clean.** Archive first (exactly `backup.sh`'s tar), write the instance row and unit without touching the running server, then one supervised cutover: `screen -X stuff 'save'` → wait → `stuff 'quit'` → wait for the JVM to actually exit → `systemctl disable --now zomboid.service` → `systemctl enable --now gvm@pz.service`. Adoption never rewrites `Saves/` and never runs `steamcmd validate` on the first pass.

The one real cost: the runner is now safety-critical code in the shutdown path, and a runner panic during a save is worse than a screen session dying. Mitigate with a `recover()` around the supervise loop that falls back to the signal ladder, and treat "runner exited while game still alive" as a hard alert.

## Sources

- https://minecraft.wiki/w/RCON
- https://pkg.go.dev/github.com/gorcon/rcon
- https://github.com/gorcon/rcon/blob/master/rcon.go
- https://mctools.readthedocs.io/en/master/rcon.html
- https://github.com/mrpaws/python-rcon-mc/issues/1
- https://python-valve.readthedocs.io/en/0.1.0/rcon.html
- https://man7.org/linux/man-pages/man5/systemd.service.5.html
- https://man7.org/linux/man-pages/man5/systemd.exec.5.html
- https://man7.org/linux/man-pages/man5/systemd.kill.5.html
- https://man7.org/linux/man-pages/man5/systemd.resource-control.5.html
- https://man7.org/linux/man-pages/man3/sd_notify.3.html
- https://man7.org/linux/man-pages/man3/sd_listen_fds.3.html
- https://man7.org/linux/man-pages/man5/journald.conf.5.html
- https://man7.org/linux/man-pages/man8/systemd-journal-gatewayd.service.8.html
- https://www.man7.org/linux/man-pages/man5/org.freedesktop.systemd1.5.html
- https://systemd.io/FILE_DESCRIPTOR_STORE/
- https://pkg.go.dev/github.com/coreos/go-systemd/v22/dbus
- https://pkg.go.dev/github.com/coreos/go-systemd/v22/daemon
- https://pkg.go.dev/github.com/coreos/go-systemd/v22/activation
- https://pkg.go.dev/github.com/coreos/go-systemd/v22/sdjournal
- https://pkg.go.dev/github.com/creack/pty
- https://pkg.go.dev/os/exec
- https://raw.githubusercontent.com/golang/go/master/src/syscall/exec_linux.go
- https://github.com/pterodactyl/wings/blob/develop/environment/docker/container.go
- https://steamcommunity.com/app/108600/discussions/6/3812906855248680121/
- https://github.com/lloesche/valheim-server-docker/issues/625
- https://blog.kuiper.dev/minecraft-server-sigterm-sigint
- https://github.com/systemd/systemd/issues/10545
- https://github.com/opencontainers/runc/blob/main/docs/cgroup-v2.md
- https://lwn.net/Articles/939511/
- https://github.com/jline/jline3/wiki/Terminals
- https://www.spigotmc.org/wiki/start-up-parameters/
- https://github.com/coreos/go-systemd/blob/main/README.md
- https://manpages.org/systemd-run
- https://man7.org/linux/man-pages/man2/pidfd_send_signal.2.html
- https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html
