gvm faq

The questions operators actually ask.

Is my server down if Postgres is down?
  No. Your game servers keep running, and start, stop, status, backup and update
  all keep working, with Postgres stopped. gvm is built so the control plane and
  the database can be down without touching the games. Only the dashboard and
  the searchable audit history need Postgres.

Why does `gvm start` or `gvm stop` wait?
  Starting waits until the server is actually up, not just launched, so "started"
  means you can connect. Stopping asks the game to save and shut down cleanly
  and waits for it — the point is to never lose a world to a hard kill. A slow
  save is the command doing its job, not hanging. `gvm status <name>` shows the
  stage.

Where are my backups?
  Under /var/backups/gvm/<instance> by default, or wherever you set backup.dir
  for that instance. `gvm backup list <name>` shows them with their class, and
  `gvm backup verify <name>` checks one is readable. A backup kept forever
  (never pruned) is a "preserve" archive — see `gvm help concepts`.

Does gvm touch my firewall?
  No. See `gvm help firewall`.

How do I install gvm on a new box?
  `sudo gvm host init`, then `sudo gvm doctor`. host init prints the apt line
  each game family needs; you run those. See `gvm help concepts` for the words
  it uses.

What do the exit codes mean?
  `gvm help exit-codes`.
