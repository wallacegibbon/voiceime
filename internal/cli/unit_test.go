package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// A model service on this machine is recognized by what it runs. Its name belongs to whoever
// installed it, the audio.cpp config it serves is whatever its own command line names, and a
// unit running anything else is not a model service at all. None of those three facts are
// assumed here, which is what lets a model installed by another project - or by hand - be
// described correctly.
func TestModelServiceIsFoundByWhatItRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	dir := config.UserUnitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("rsync-backup.service", "[Unit]\nDescription=nightly\n\n[Service]\nExecStart=/usr/bin/rsync --daemon\n")
	write("session-agent.service", "[Service]\nExecStart=/home/u/.local/bin/agent --config /home/u/.config/agent.json\n")

	const binary = "/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server"
	const served = "/home/u/.local/share/audiocpp-asr/serve.json"
	write("audiocpp-asr.service", "[Unit]\nDescription=a model on this machine\n\n[Service]\nExecStart="+binary+
		" --config "+served+" --log\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n")

	service, found := modelServiceInstalled()
	if !found {
		t.Fatal("no model service found, want the unit that runs audio.cpp")
	}
	if service.Unit != "audiocpp-asr.service" {
		t.Errorf("found %q, want the unit named for what it runs", service.Unit)
	}
	if service.Binary != binary || service.Config != served {
		t.Errorf("found %+v, want the binary and the config that ExecStart names", service)
	}

	// A machine with no such unit is not missing anything: it has no model here.
	if err := os.Remove(filepath.Join(dir, "audiocpp-asr.service")); err != nil {
		t.Fatal(err)
	}
	if service, found := modelServiceInstalled(); found {
		t.Errorf("found %+v, want nothing: the units here run other things", service)
	}
}
