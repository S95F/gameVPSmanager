# Proton: Windows game servers on Linux

Some game servers ship only as Windows binaries. gvm runs them on Linux through
Proton, Valve's Wine-based compatibility layer. V Rising, Enshrouded, ARK:
Survival Evolved and Satisfactory are the catalogue's Proton titles; `gvm game
list` and `gvm game show <id>` mark them.

## gvm never installs Proton for you

gvm names prerequisites; it never downloads or installs them — the same rule
that stops it running `apt` or enabling a firewall behind your back. Proton is
large, versioned, and a thing you choose, so you install it and point gvm at it.

## Give each instance its own Proton

Install a Proton build yourself, then set `proton_path` when you create the
instance:

    sudo gvm create vr1 --game vrising --var proton_path=/srv/gvm/vr1/work/proton

The copy must be owned by that instance's own user (`gvm-vr1`). A Proton tree
that several instances share must not be writable by any instance user: one
compromised mod must never be able to rewrite the loader another server runs
through. gvm's own sandbox stays on, so keep `proton_path` out of a home
directory: the unit's `ProtectHome=` would hide it from the game, and the spawn
preflight fails fast (exit 78) rather than starting a server that cannot read
its own loader.

## What to expect

- Proton needs W^X relaxed (`MemoryDenyWriteExecute=no`) and a 32-bit syscall
  surface. The shipped manifests set this, and the generated drop-in says why.
- A Proton server keeps its Wine prefix under `STEAM_COMPAT_DATA_PATH`. Whether
  that prefix also holds the world depends on the game; `gvm backup` follows the
  manifest's world paths either way.
- A stop saves through the game's own save-and-quit over the control channel
  where one exists, not a bare signal to Wine.

A native Linux server is always preferable where one exists, but `satisfactory`
and `ark-se` currently run under Proton like the titles above — no native
manifest ships yet.
