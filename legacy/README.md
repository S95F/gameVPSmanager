# legacy/

Reference material, not built or shipped.

## `projectzomboid-bash/`

The original single-game Project Zomboid Build 42 installer suite that
`gameVPSmanager` generalizes. It is vendored here verbatim (minus secrets) for
three reasons:

1. **It is the specification for the Project Zomboid game definition.** Every
   behaviour these scripts implement — the two-list mod problem
   (`WorkshopItems` numeric IDs vs internal `Mods` string IDs read out of
   `mod.info`), B41-vs-B42 disambiguation, sandbox preset + per-key overrides,
   archive-before-destroy world rebuild, the clean `save` → `quit` shutdown
   sequence — has to survive in the new manager.

2. **It is what the live server is currently running.** `gvm adopt` imports an
   install created by these scripts. The paths, unit name, unix user and file
   layout here are the contract that adoption has to match.

3. **It documents operational lessons that are expensive to relearn** — see
   `projectzomboid-bash/README.md`, which is unusually detailed about failure
   modes.

**Do not edit these files to change behaviour.** Change the Go implementation
and its game manifest instead. This tree only gets touched to keep it in sync
with what the live server actually runs.

### Scrubbed

`config.env` had a real join password in it; it has been replaced with a
placeholder. The live values stay on the server.
