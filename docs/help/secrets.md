gvm secrets

What gvm treats as a secret, and the rules it keeps.

A secret is a password or a key a game or gvm needs but that must not sit in a
world file, an argv or the audit log. `gvm game show <id>` lists the secrets a
game references (by name — never a value).

Instance secrets vs host secrets
  An instance secret reaches a game: its RCON or admin password, a join
  password. gvm refuses to store one containing a character it cannot carry
  into the game's command line or config files unaltered — a quote, a
  backslash, a dollar sign, a backtick, a semicolon, an equals sign, or
  whitespace. Such a value would be cut, escaped or re-interpreted before the
  game saw it, so gvm names the offending position (never the value) and asks
  you to rotate it to one without that character.

  A host secret (an API key, set with --host) is sent only in an HTTP header,
  never into a game, so it keeps only the file-format rules: not empty, and no
  newline. A key like `$2a$10$…` is fine as a host secret and would be refused
  as an instance one.

How gvm handles a value
  A value is never read from the command line (anyone can see argv). It comes
  from a no-echo prompt, from stdin, from a file, or is generated. Stored files
  are 0600 root:root. gvm prints only a length and a fingerprint, and the audit
  log records that a secret was written or read, with its name — never its
  value.

gvm never alters or invents an adopted instance's secrets; its values arrive
only through the adoption import, and `gvm secret rotate` refuses to touch one.

Setting, reading and rotating secrets (root, on the host)
  sudo gvm secret set <instance> <name>      prompt, pipe, --from-file or --generate
  sudo gvm secret set --host <name>          a box-wide key (e.g. curseforge)
  sudo gvm secret list <instance>            names, lengths and fingerprints
  sudo gvm secret show <instance> <name>     print one value, at a terminal only
  sudo gvm secret rotate <instance> <name>   replace it with a fresh password
  sudo gvm secret show-path <instance> <name>  where the file lives

A stored value does nothing until the game reads it, and nothing restarts on
its own: `set` and `rotate` print the `sudo gvm restart <instance>` that puts a
new value in force. Generating missing secrets during `gvm create` and writing
them into a game's own config files land in later phases.
