package hook

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// palSettingsSeed is pal.settings_seed.
//
// Palworld ships DefaultPalWorldSettings.ini in its install tree and reads
// Pal/Saved/Config/LinuxServer/PalWorldSettings.ini at boot, and it does not
// create the second from the first. A server started without it comes up with
// every setting at its compiled-in default: no name, no password, the wrong
// player cap -- and nothing in the logs says why, because from the server's
// point of view nothing went wrong.
//
// This is a seed and not a template. The file gvm writes is the server's OWN
// default file, byte for byte; the manifest's owned keys are then applied to it
// by the normal config path, which is what makes "values are written bare or
// quoted exactly as the seeded file already spells each option" possible.
// Generating the file from a table instead would mean inventing the quoting --
// and PublicPort=8211 is a count while ServerName="8211" is a name, with
// nothing in the value itself to tell them apart.
type palSettingsSeed struct{}

func (palSettingsSeed) Name() string    { return "pal.settings_seed" }
func (palSettingsSeed) Phases() []Phase { return []Phase{PhasePostInstall} }

func (palSettingsSeed) Doc() string {
	return "Copies the install tree's DefaultPalWorldSettings.ini into " +
		"Pal/Saved/Config/LinuxServer/PalWorldSettings.ini on first run. Palworld reads the " +
		"second and generates neither, so without this the server boots with every setting at " +
		"its compiled-in default and says nothing about it. Never overwrites an existing file."
}

// palDefaultSettings is where Palworld ships its default, relative to the
// install tree.
const palDefaultSettings = "DefaultPalWorldSettings.ini"

// palSettingsTarget is the file the server actually reads, relative to the
// install tree. Palworld's save tree lives inside its install tree, which is
// why palworld.yaml's backup.include names {{ .Install }}/Pal/Saved and not
// {{ .Data }}.
const palSettingsTarget = "Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"

func (palSettingsSeed) Run(_ context.Context, h *Ctx) error {
	return seedFromInstall(h, seedSpec{
		Source:     palDefaultSettings,
		ConfigName: "settings",
		FallbackTo: palSettingsTarget,
		Why: "Palworld reads PalWorldSettings.ini and generates it from nothing; without the " +
			"seed every setting is at its compiled-in default",
	})
}

// seedSpec is one "copy the server's own shipped default into place" operation.
//
// Both games that need this need the same five decisions -- which source, which
// declared target, which mode, never overwrite, and report rather than fail
// when the source is missing -- so they are one function.
//
// The source is a per-game constant because where a game ships its default IS
// game knowledge, which is the whole reason this is a hook. The TARGET is not:
// it is read out of the manifest's config_files entry, so moving a config file
// in a manifest revision cannot leave a hook writing the shipped default to the
// old location while everything else reads the new one.
type seedSpec struct {
	// Source is the shipped default, relative to the install tree.
	Source string
	// ConfigName is the manifest's config_files[].name for the target.
	ConfigName string
	// FallbackTo is the install-relative target for a manifest that does not
	// declare the file. It exists for the generic case, not for the shipped
	// manifests, which all declare it.
	FallbackTo string
	Why        string
}

// seedFromInstall copies a shipped default into place, once.
//
// Idempotent by NOT overwriting. That is the whole of the rule: the target is a
// file the game and the operator both edit afterwards, so a hook that re-copied
// the default on every post_install would silently revert a server's settings
// the next time anybody ran a repair. Contract rule 2 says a hook must be
// idempotent, and for a seed "idempotent" means "does nothing the second time",
// not "produces the same bytes".
func seedFromInstall(h *Ctx, s seedSpec) error {
	fsys, err := h.fsOf()
	if err != nil {
		return err
	}

	target, rel, mode, err := ConfigTarget(h, s.ConfigName)
	if err != nil {
		if !errors.Is(err, ErrConfig) || s.FallbackTo == "" {
			return err
		}
		// The manifest does not declare the file. That is legitimate for a
		// hand-written manifest and it has to be said out loud, because the
		// owned keys will not be applied to whatever gets seeded.
		h.Note("this manifest declares no config file called %q, so the shipped default goes "+
			"to its conventional location (%s) and no owned keys will be applied to it",
			s.ConfigName, s.FallbackTo)
		target, rel, mode = RootInstall, s.FallbackTo, 0o640
	}

	if _, err := fsys.Stat(target, rel); err == nil {
		h.Note("%s/%s is already there; the shipped default was not copied over it", target, rel)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hook: checking %s/%s: %w", target, rel, err)
	}

	data, err := fsys.ReadFile(RootInstall, s.Source)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A problem and not an error: the install may be mid-repair, or
			// this build of the game may ship the default somewhere else, and
			// an aborted post_install phase would leave an instance that is
			// otherwise installed in a failed state. Doctor shows this.
			h.Problem("%s is not in the install tree, so %s was not seeded. %s",
				s.Source, rel, s.Why)
			return nil
		}
		return fmt.Errorf("hook: reading %s: %w", s.Source, err)
	}
	if len(data) == 0 {
		h.Problem("%s is empty, so %s was not seeded from it", s.Source, rel)
		return nil
	}

	if err := fsys.MkdirAll(target, path.Dir(rel), dirModeFor(mode)); err != nil {
		return err
	}
	if err := fsys.WriteFile(target, rel, data, mode); err != nil {
		return err
	}
	h.Note("seeded %s/%s from %s (%d bytes)", target, rel, s.Source, len(data))
	return nil
}
