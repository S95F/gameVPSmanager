gvm troubleshooting

Start here when something is wrong.

`gvm doctor`
  The first command to run. It checks the box and every instance and prints
  what is wrong, most serious first, each with the fix. It exits 4 when it found
  problems, so a script can tell "healthy" from "look at this".

A command says it "needs root"
  Run it again with sudo. gvm prints the exact line to paste.

A command says something is "not available in this build"
  That feature is not shipped yet. gvm changed nothing. The message names what
  is missing; there is nothing to fix on your box.

A server will not start
  `gvm status <name>` shows the last thing that happened and why. `gvm logs
  <name>` prints the server's own console output and the record of its last
  shutdown. Most "it won't start" turns out to be a port already in use
  (`gvm ports`) or a config the server itself rejected, which is in the logs.

`gvm stop` seems to hang
  It is climbing the stop ladder: asking the game to save and exit, then
  waiting. A busy server can take a minute to save. `gvm status <name>` in
  another terminal shows the stage it is at.

Postgres is down and commands still work
  That is by design. Game servers, backups and updates do not depend on
  Postgres. Only the dashboard and the audit history do. See `gvm help faq`.
