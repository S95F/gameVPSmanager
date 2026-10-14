package modrinth

// env.go handles Modrinth's environment compatibility field.
//
// Modrinth mods declare client/server compatibility as "required" | "optional"
// | "unsupported". A mod that is "unsupported" on the server side must not be
// installed. gvm defaults to filtering for server-compatible mods only.

// modrinthEnv is the environment compatibility declaration in a Modrinth project.
type modrinthEnv struct {
	Client string `json:"client"` // "required" | "optional" | "unsupported"
	Server string `json:"server"` // "required" | "optional" | "unsupported"
}

// serverCompatible reports whether a mod is compatible with server-side use.
// "unsupported" on server means the mod must not be installed server-side.
func serverCompatible(env modrinthEnv) bool {
	return env.Server != "unsupported"
}

// clientOnly reports whether a mod is client-only (unsupported on server).
func clientOnly(env modrinthEnv) bool {
	return env.Server == "unsupported" && env.Client == "required"
}
