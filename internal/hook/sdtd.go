package hook

import "context"

// sdtdSeedConfig is sdtd.seed_config.
//
// 7 Days to Die ships serverconfig.xml in its install tree and reads the one it
// is pointed at on the command line. gvm points it at {{ .Data }} -- outside the
// install tree, so a `validate` or a reinstall cannot overwrite an operator's
// settings, which is the whole reason the data tree exists separately -- and
// nothing creates the file there.
//
// Unlike Palworld's, this file is `owner: gvm`: the server reads it at boot and
// never writes it back. That makes the seed a one-time act with no race against
// the game, and it is why the manifest can own every key in it.
type sdtdSeedConfig struct{}

func (sdtdSeedConfig) Name() string    { return "sdtd.seed_config" }
func (sdtdSeedConfig) Phases() []Phase { return []Phase{PhasePostInstall} }

func (sdtdSeedConfig) Doc() string {
	return "Copies the install tree's serverconfig.xml into the data tree, where gvm points " +
		"the server at it. Keeping it outside the install tree is what stops a reinstall or a " +
		"SteamCMD validate from overwriting an operator's settings; nothing else puts it " +
		"there. Never overwrites an existing file."
}

// sdtdShippedConfig is where 7 Days to Die ships its default, relative to the
// install tree.
const sdtdShippedConfig = "serverconfig.xml"

func (sdtdSeedConfig) Run(_ context.Context, h *Ctx) error {
	return seedFromInstall(h, seedSpec{
		Source:     sdtdShippedConfig,
		ConfigName: "serverconfig",
		// No fallback. The whole point is that the file lives OUTSIDE the
		// install tree, so a manifest that does not declare where has nowhere
		// for this hook to put it, and the conventional location would be the
		// source file itself.
		Why: "7 Days to Die is launched with -configfile pointing at the data tree, and a " +
			"missing file there makes the server fall back to its built-in defaults: a " +
			"different world, a different name and no admin",
	})
}
