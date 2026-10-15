package hostinit

// AptHint is one game family and the apt command that prepares a host for it.
//
// gvm PRINTS these and never runs them (rule 7). Installing distribution
// packages is the operator's decision and their package manager's job: a
// control plane that ran `apt install` behind an operator's back would be the
// first surprising thing it did, and on a box where apt is held or mirrored it
// would be the first broken one. Printing the exact line keeps the command a
// copy-paste without pretending gvm owns the system's packages.
type AptHint struct {
	Family string
	// Why is the one-line reason the packages are needed, so an operator can
	// decide whether a family applies to them before pasting anything.
	Why string
	// Cmd is the exact shell line to run. It is DATA printed to the operator,
	// never passed to a shell by gvm.
	Cmd string
}

// AptHints is the per-family preparation, in the order printed.
//
// SteamCMD is first because it is the common case and the one with the
// non-obvious step: the i386 architecture has to be added before the 32-bit
// runtime can be installed, and an operator who installs lib32gcc-s1 without it
// gets an error that does not say so.
var aptHints = []AptHint{
	{
		Family: "SteamCMD titles (Project Zomboid, Valheim, Rust, …)",
		Why:    "steamcmd is a 32-bit binary and needs the i386 runtime",
		Cmd:    "dpkg --add-architecture i386 && apt update && apt install -y lib32gcc-s1",
	},
	{
		Family: "Minecraft (Java edition)",
		Why:    "the server is a Java application",
		Cmd:    "apt install -y openjdk-21-jre-headless",
	},
	{
		Family: "Proton titles",
		Why:    "Proton's launch wrapper is driven from Python",
		Cmd:    "apt install -y python3",
	},
	{
		Family: "titles that need a virtual display",
		Why:    "a headless server that still opens an X display needs a fake one",
		Cmd:    "apt install -y xvfb",
	},
}

// AptHints returns the preparation hints in print order.
func AptHints() []AptHint { return append([]AptHint(nil), aptHints...) }
