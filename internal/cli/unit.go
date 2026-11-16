package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// A model on this machine is not this program's. It is a systemd user unit that runs an
// audio.cpp build, and it belongs to whatever installed it: the unit's name, the name and place
// of the audio.cpp config it is handed, and the checkpoint behind that are that project's
// business. None of them are settings this program reads, and none of them are named here.
//
// So a model service is found by what it runs, and what it was told to serve is read out of its
// own `ExecStart`. That is what keeps the two halves usable apart: a machine with only a
// provider's address has no such unit and nothing below says anything about one, and a machine
// whose model service was installed by something else is still described truthfully.

// modelServiceBinary is the executable a model service runs. It is the one name shared with the
// project that installs such a thing, and it is how a model service is recognized.
const modelServiceBinary = "audiocpp_server"

// installModelService is the command that puts a model on this machine where the settings can
// post to it. It belongs to the project that installs model services, and it is written here
// without a path: this program does not own that checkout, and a path it invented would be a
// guess about someone else's machine.
const installModelService = "asr-service.sh <audiocpp_server> <model.gguf>"

// modelService is a model service as this machine describes it.
type modelService struct {
	// Unit is the name systemd knows it by, and UnitFile the path it was read from.
	Unit     string
	UnitFile string
	// Binary and Config are what its own ExecStart line says it runs and what it was handed
	// to serve them with. They are read, never derived: a moved checkout or a renamed config
	// is a fact about the file, and the only way to know it is to look.
	Binary string
	Config string
}

// modelServiceInstalled finds the unit on this machine that runs audio.cpp. More than one is
// possible and the first in name order is the one described: two of them is a question about
// this machine for the person who set it up, not something to choose between here.
func modelServiceInstalled() (modelService, bool) {
	dir := config.UserUnitDir()
	if dir == "" {
		return modelService{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return modelService{}, false
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".service") {
			continue
		}
		path := filepath.Join(dir, name)
		fields, err := unitExecStart(path)
		if err != nil || len(fields) == 0 {
			continue
		}
		binary := firstField(fields)
		if filepath.Base(binary) != modelServiceBinary {
			continue
		}
		return modelService{Unit: name, UnitFile: path, Binary: binary, Config: unitArgument(fields, "--config")}, true
	}
	return modelService{}, false
}

// serviceState answers what systemd knows and a unit file does not: would this unit start at
// login, and is it up now. A unit that is neither is a record someone can start again, and a
// warning about it is noise.
//
// It is a function rather than a call because a check about files should not need a running
// system to be tested.
type serviceState func(unit string) (startsAtLogin, up bool)

// systemdServiceState asks systemctl, once per question. An unanswerable question - no systemd,
// a unit systemd has never heard of - reads as a unit that is not running and will not start.
func systemdServiceState(unit string) (startsAtLogin, up bool) {
	enabled, err := probe("systemctl", "--user", "is-enabled", unit)
	startsAtLogin = err == nil && strings.HasPrefix(strings.TrimSpace(enabled), "enabled")
	active, err := probe("systemctl", "--user", "is-active", unit)
	up = err == nil && strings.TrimSpace(active) == "active"
	return startsAtLogin, up
}
