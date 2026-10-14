package steamcmd

// PlatformWindows is re-exported here so manifests that configure a Windows
// depot do not need to import internal/spec just for this string.
//
// Setting SteamCMDSpec.Platform to PlatformWindows (or spec.PlatformWindows)
// causes runscript.go to emit:
//
//	@sSteamCmdForcePlatformType windows
//
// This is how games that only publish a Windows dedicated server binary are
// installed on Linux hosts. The depot download happens in a transient
// service unit, so the Windows binaries land in the install directory without
// any Wine or Proton involved at the SteamCMD stage.
//
// Games that need this:
//   - ARK: Survival Evolved (appid 376030)
//   - Satisfactory (appid 1690800)
//   - V Rising (appid 1604030)
//   - Enshrouded (appid 2278520)
const PlatformWindows = "windows"
