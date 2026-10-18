gvm exit codes

Every operator command exits with one of these. A script can branch on the
number without parsing the message.

  0  ok           the command did what it said.
  1  failed       the action was attempted and did not succeed.
  2  usage        the command was typed wrong — an unknown subcommand, a
                  missing argument. Nothing was attempted.
  3  not here     the thing you asked for is not in this build. Nothing was
                  changed. The message names what is missing.
  4  found it     the command looked and found something worth your attention
                  without failing: doctor problems, `list --problems` with a
                  hit, a port conflict, an available update. The result is in
                  the output.
  5  refused      the command declined to act: a typed-name confirmation that
                  did not match, a precondition you must clear first. Nothing
                  was changed.

The scheduled job runner is the one exception. `gvm job run`, when systemd runs
it as a unit, maps its result to the unit's contract instead: a routine skip or
a degraded pass exits 0 so the unit is not marked failed, a real failure exits
1, and a job this build cannot run exits 3. `gvm run`, which supervises a
server for systemd, keeps its own exit codes so systemd's restart policy reads
them correctly.
