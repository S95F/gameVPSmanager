gvm concepts

A short glossary of the words gvm uses in its output.

the ladder
  How gvm stops a server: it asks the game to save and shut down cleanly, waits,
  and only escalates to a harder stop if the game does not go. A stop you start
  is the top of the ladder; gvm climbs down it for you.

hold
  A flag you set to say "leave this instance alone". A held instance is skipped
  by the nightly jobs and by the guard, and gvm says so rather than acting. You
  clear the hold when you want automation back.

lease
  A short-lived claim one job takes so two jobs never touch the same world at
  once. A backup takes a freeze lease while it copies; an update takes one while
  it stops the server. A lease expires on its own, so a crashed job never wedges
  the next one.

preserve archive
  A backup that retention will never delete. A routine nightly is pruned once it
  is old enough; a preserve archive is kept until you remove it yourself. The
  archive gvm takes before a destroy is pinned this way, so the thing you were
  trusting is still there afterwards.

drift
  The difference between what an instance's file says it should be and what is
  actually on the box. `gvm apply` closes drift; `gvm apply --diff` shows it
  without changing anything.

pin
  A value written down so a later run cannot silently change it — the game build
  an instance is on, or the preserve mark on an archive. A pin is what makes
  "the same thing as last time" mean the same thing.
