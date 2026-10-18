# The dashboard, for moderators (v0)

You were given a dashboard account so you can look after game servers without
SSH. This page covers what works today.

## Getting an account

The operator sends you a single-use invite link. Open it, choose your own
password (at least 12 characters), add the shown secret to your authenticator
app, and prove one code. Nobody else ever knows your password or your
authenticator secret — if either is lost, the operator resets the account and
sends a fresh link.

## Signing in

1. Open the dashboard address your operator gave you.
2. Enter your email and password, then the 6-digit code from your
   authenticator app. There is no way around the code: no authenticator, no
   session. If you lost it, the operator resets it from the host
   (`sudo gvm user reset you@example.com --totp`) and you enrol again.
3. Five wrong passwords lock the account for fifteen minutes. The lock clears
   itself; a reset is only needed for a lost factor.

Sessions last 12 hours; **Sign out** ends one early.

## What you see

Every server on the host, each with its state in plain words — "Running.",
"Starting up and loading the world…", "This server keeps crashing as it
starts. The server operator needs to check the logs." When something needs
the machine's owner, the sentence says so; you are never handed a shell
command you cannot run.

## What you can do

Depends on your role (ask the operator; `gvm user roles` on the host prints
the whole table):

- **viewer** — look at everything above, change nothing.
- **operator** — the moderator role: **Start**, **Stop** and **Restart**
  buttons on the servers you were granted. Stop and Restart walk the game's
  save ladder, exactly like the operator's own `sudo gvm stop` — the world is
  saved first, every time.
- A button you cannot use is refused with the reason, not hidden behind a
  silent failure. While a backup or update holds a server, lifecycle actions
  answer "a job holds this server" with the job's own description — try again
  when it finishes.

Restore, rebuild, mod edits and settings are not in the dashboard in this
release, by design: the actions with no undo stay with the operator.

## The console

The console page streams the server's output live, with the last 200 lines
replayed when you open it. A dropped connection resumes itself where it left
off; when some lines are gone for good it says "— N lines skipped —" rather
than pretending nothing happened. A stopped server shows its state above the
pane instead of a dead feed.

With the operator role you also get a send box. It takes one command at a
time, and it refuses `quit`, `stop`, `shutdown` and `exit` outright: **use
the Stop button** — that path saves the world first, and a typed `quit` does
not. Everything you send is recorded with your name in the server's own
console log.

## When the dashboard is down

Nothing happens to the game servers. They are supervised by systemd, not by
the dashboard, and the operator can do everything over SSH. If you see the
"datastore is down" page, the servers are still running — tell the operator
and carry on playing.
