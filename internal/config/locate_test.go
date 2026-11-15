package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A built audio.cpp CLI is something to find once, when the settings are written, and to
// name afterwards. Nothing here is consulted while dictating: adding a second build tree
// must not change which inference a running service performs.

// buildTree writes an audio.cpp build tree under root and returns the CLI path.
func buildTree(t *testing.T, root, profile string, executable bool) string {
	t.Helper()
	cli := filepath.Join(root, "build", profile, "bin", "audiocpp_cli")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if executable {
		sibling := filepath.Join(filepath.Dir(cli), serverBinaryName)
		if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cli
}

// clearPATH makes sure nothing on the real PATH can answer, so the only possible source is
// what the settings name.
func clearPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestFindASRBinaryRefusesToGuess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	built := buildTree(t, filepath.Join(home, "audio.cpp"), "linux-cpu-release", true)

	// A CLI sitting in the most obvious place on the machine is still not an answer:
	// discovery belongs to `config init`, which writes it into asr_bin and says so.
	cfg := Default()
	clearPATH(t)
	if _, err := cfg.FindASRBinary(); err == nil {
		t.Errorf("FindASRBinary found %s with asr_bin empty; a build tree added after the settings were written changed the answer", built)
	} else if !strings.Contains(err.Error(), "asr_bin") {
		t.Errorf("error = %q, want it to name the setting to set", err)
	}

	// Named: that exact file.
	cfg.ASRBin = built
	found, err := cfg.FindASRBinary()
	if err != nil || found != built {
		t.Errorf("FindASRBinary = %q, %v, want %q", found, err, built)
	}

	// Named and not there: the answer names the path that was configured, because the
	// settings being wrong is a different problem from audio.cpp not existing.
	cfg.ASRBin = filepath.Join(home, "audio.cpp", "build", "gone-release", "bin", "audiocpp_cli")
	if _, err := cfg.FindASRBinary(); err == nil {
		t.Error("FindASRBinary accepted an asr_bin that is not a file")
	}

	// A file that is there but not runnable (a source tree copy, a directory) is the same
	// answer: the settings name something that cannot run.
	notRunnable := filepath.Join(home, "audiocpp_cli")
	if err := os.WriteFile(notRunnable, []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ASRBin = notRunnable
	if _, err := cfg.FindASRBinary(); err == nil {
		t.Error("FindASRBinary accepted a file that is not runnable")
	}
}

// The server is not a second thing to locate: audio.cpp builds it beside the CLI, so one
// recorded path covers both.
func TestFindServerBinaryIsTheSiblingOfTheNamedCLI(t *testing.T) {
	root := t.TempDir()
	cli := buildTree(t, root, "linux-cpu-release", true)
	cfg := Default()
	cfg.ASRBin = cli

	want := filepath.Join(filepath.Dir(cli), serverBinaryName)
	found, err := cfg.FindServerBinary()
	if err != nil || found != want {
		t.Errorf("FindServerBinary = %q, %v, want %q", found, err, want)
	}

	// A build of the CLI without the server beside it is called out as such.
	orphan := buildTree(t, t.TempDir(), "linux-cpu-release", false)
	cfg.ASRBin = orphan
	if _, err := cfg.FindServerBinary(); err == nil {
		t.Error("FindServerBinary accepted a build with no server beside it")
	} else if !strings.Contains(err.Error(), "beside") {
		t.Errorf("error = %q, want it to say the server is not beside the CLI", err)
	}
}

// What `config init` searches for: built CLIs under the plausible checkout locations.
func TestDiscoverASRBinariesFindsBuiltCLIs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, "playground", "audio.cpp")
	buildTree(t, checkout, "linux-cpu-release", true)
	buildTree(t, checkout, "linux-cuda-release", true)
	buildTree(t, checkout, "linux-notbuilt-release", false) // configured but never built
	buildTree(t, filepath.Join(home, "audio.cpp"), "build-me", true)

	found := DiscoverASRBinaries()
	if len(found) != 3 {
		t.Fatalf("DiscoverASRBinaries = %v, want the three runnable CLIs", found)
	}
	// The order is the candidate list's, and within one checkout the build profiles in
	// name order: stable, so two runs of `config init` point asr_bin at the same file.
	// A checkout with several build trees returns all of them, because that is a decision
	// for the person writing the settings, not a guess for this function.
	want := []string{
		filepath.Join(home, "audio.cpp", "build", "build-me", "bin", "audiocpp_cli"),
		filepath.Join(checkout, "build", "linux-cpu-release", "bin", "audiocpp_cli"),
		filepath.Join(checkout, "build", "linux-cuda-release", "bin", "audiocpp_cli"),
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("DiscoverASRBinaries[%d] = %s, want %s", i, found[i], want[i])
		}
	}
	for _, path := range found {
		if strings.Contains(path, "notbuilt") {
			t.Errorf("DiscoverASRBinaries returned %s, which is not runnable", path)
		}
	}
}

func TestDiscoverASRBinariesSaysNothingWhenThereIsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if found := DiscoverASRBinaries(); len(found) != 0 {
		t.Errorf("DiscoverASRBinaries = %v, want nothing on a machine without audio.cpp", found)
	}
}
