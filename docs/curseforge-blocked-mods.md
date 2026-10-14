# CurseForge Blocked Mods

Some mod authors opt out of third-party API distribution. When a mod is
blocked, the CurseForge API returns an empty `downloadUrl` for its file.

## How gvm handles blocked mods

gvm never synthesises a CDN URL. Every download URL must come verbatim from
the API response. When the API returns an empty `downloadUrl`, the mod is
**blocked** — the author has disabled automated download for their file.

The resolution ladder handles blocked mods as follows:

1. **api** — The API returned a non-empty `downloadUrl`. The file is downloaded
   directly from that URL. No URL is ever constructed from project/file IDs.

2. **cache** — A previously downloaded copy of the file (matched by murmur2
   fingerprint) is already in the blob store.

3. **manual** — The operator placed the JAR file in the *drop-box* directory
   (`/etc/gvm/curseforge/dropbox/`). gvm matches it by murmur2 fingerprint or,
   as a fallback, by filename stem.

4. **mirror\_modrinth** — The mod is also published on Modrinth under the same
   project ID (stub; not yet implemented).

5. **blocked** — None of the above rungs succeeded. The mod is marked as
   **blocked** in the content plan with a `nil` artifact. The plan continues;
   other mods are not affected.

## The drop-box workflow

When a modpack contains a blocked mod:

1. Run `gvm content plan <instance>`. The plan output lists blocked mods with
   their CurseForge project ID and file ID.

2. Download the JAR manually from the CurseForge website (requires a CurseForge
   account) or from a mirror the author has authorised.

3. Place the JAR in `/etc/gvm/curseforge/dropbox/`. The filename must end in
   `.jar`; the stem should match the CurseForge file name (used as a fallback
   match after the murmur2 fingerprint check).

4. Re-run `gvm content plan <instance>`. The blocked entry will now resolve via
   the **manual** rung.

## Security

- The `x-api-key` header is operator-supplied via the gvm credential store. It
  is never embedded in manifests, YAML files, or source code.
- The nosynth test (`internal/content/curseforge/nosynth_test.go`) asserts that
  no string literal in the curseforge package contains `forgecdn.net` or
  `edge.forgecdn`. This ensures no CDN URL can be synthesised from file IDs.
- Blocked mods with a `nil` artifact are recorded in the plan and skipped during
  materialise; they do not abort the pack deployment.

## Affected games

Well-known mods that are commonly blocked include some mods in the Feed the
Beast (FTB) modpacks. Check the plan output after adding a modpack; blocked
entries are clearly labelled.
