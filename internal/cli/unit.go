package cli

import (
	"path/filepath"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The two files a model on this machine consists of: the systemd unit and the audio.cpp
// config it names. scripts/asr-service.sh writes them; they are recorded here because the
// checks that read them back - does the unit still point at a binary that exists, does that
// binary still get a config that exists - have to look at the same two paths.
const (
	unitName = "voiceime-asr-server.service"
	// modelServiceBinary is the executable the unit runs. It is named here because the
	// check that looks for the running service greps process command lines for it.
	modelServiceBinary  = "audiocpp_server"
	generatedConfigName = "asr-server.json"
)

func unitPath() string {
	dir := config.UserUnitDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, unitName)
}

// generatedConfigPath is in the data directory rather than the runtime one because the unit
// starts at login: a file in a directory emptied at logout would not be there when systemd
// asks for it.
func generatedConfigPath() string {
	return filepath.Join(config.DataDir(), generatedConfigName)
}
