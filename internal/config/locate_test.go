package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Which audio.cpp runs is asked once, by `voiceime apply`, and the answer goes into the
// service unit. Nothing here is consulted while dictating: adding a second build tree must
// not change which inference a running service performs.

// buildTree writes an audio.cpp build tree under root and returns the server path. The CLI
// beside it is written too, because a real build has both and the server must be the one
// found.
func buildTree(t *testing.T, root, profile string, executable bool) string {
	t.Helper()
	bin := filepath.Join(root, "build", profile, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	for _, name := range []string{"audiocpp_cli", ASRServerName} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(bin, ASRServerName)
}

// clearPATH makes sure nothing on the real PATH can answer, so the only possible sources are
// the build trees this test creates.
func clearPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestASRServerRefusesToGuessWhenThereIsNoAudioCPP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearPATH(t)
	if _, _, err := ASRServer(); err == nil {
		t.Fatal("ASRServer answered on a machine without audio.cpp")
	} else if !strings.Contains(err.Error(), ASRServerName) || !strings.Contains(err.Error(), "cmake") {
		t.Errorf("error = %q, want it to name the binary and how to build it", err)
	}
}

func TestASRServerFindsABuildBeforePATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, "playground", "audio.cpp")
	built := buildTree(t, checkout, "linux-cpu-release", true)

	// The reference machine has both: an installed ~/.local/bin/audiocpp_server and a build
	// tree, same day, different builds. A checkout is the more specific answer, and the one
	// that is being developed.
	installed := filepath.Join(t.TempDir(), ASRServerName)
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(installed))

	found, source, err := ASRServer()
	if err != nil {
		t.Fatalf("ASRServer: %v", err)
	}
	if found != built {
		t.Errorf("ASRServer = %q, want the build tree's %q", found, built)
	}
	if source == "" {
		t.Error("ASRServer gave no source for its answer, so `apply` cannot say where it came from")
	}

	// With no checkout, PATH is the answer.
	os.RemoveAll(checkout)
	found, source, err = ASRServer()
	if err != nil {
		t.Fatalf("ASRServer with only an installed server: %v", err)
	}
	if found != installed || source != "PATH" {
		t.Errorf("ASRServer = %q from %q, want %q from PATH", found, source, installed)
	}
}

// A build directory that was configured but never built is not an answer: it is what the first
// `voiceime apply` on a fresh checkout looks like, and the caller has to be told to build it
// rather than handed a file systemd cannot start.
func TestASRServerIgnoresWhatCannotRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearPATH(t)
	buildTree(t, filepath.Join(home, "audio.cpp"), "linux-notbuilt-release", false)
	if _, _, err := ASRServer(); err == nil {
		t.Error("ASRServer accepted a build directory that was never built")
	}
	if found := DiscoverASRServers(); len(found) != 0 {
		t.Errorf("DiscoverASRServers = %v, want only what can be run", found)
	}
}

// What `voiceime apply` searches for: built servers under the plausible checkout locations,
// in an order that does not change between runs.
func TestDiscoverASRServersFindsBuiltServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, "playground", "audio.cpp")
	buildTree(t, checkout, "linux-cpu-release", true)
	buildTree(t, checkout, "linux-cuda-release", true)
	buildTree(t, checkout, "linux-notbuilt-release", false) // configured but never built
	buildTree(t, filepath.Join(home, "audio.cpp"), "build-me", true)

	found := DiscoverASRServers()
	if len(found) != 3 {
		t.Fatalf("DiscoverASRServers = %v, want the three runnable servers", found)
	}
	// The candidate list's order first, and within one checkout the build profiles in name
	// order: stable, so two runs of the search name the same server and report the same
	// others. Several build trees in one checkout are all reported, because choosing between
	// them is not this function's guess to make.
	want := []string{
		filepath.Join(home, "audio.cpp", "build", "build-me", "bin", ASRServerName),
		filepath.Join(checkout, "build", "linux-cpu-release", "bin", ASRServerName),
		filepath.Join(checkout, "build", "linux-cuda-release", "bin", ASRServerName),
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("DiscoverASRServers[%d] = %s, want %s", i, found[i], want[i])
		}
	}
	for _, path := range found {
		if strings.Contains(path, "notbuilt") {
			t.Errorf("DiscoverASRServers returned %s, which is not runnable", path)
		}
	}
	// The CLI in the same directory is never returned: inference goes over HTTP, so the only
	// audio.cpp that matters is the one that can serve.
	for _, path := range found {
		if strings.HasSuffix(path, "_cli") {
			t.Errorf("DiscoverASRServers returned %q, which is not the server", path)
		}
	}
}

func TestDiscoverASRServersSaysNothingWhenThereIsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if found := DiscoverASRServers(); len(found) != 0 {
		t.Errorf("DiscoverASRServers = %v, want nothing on a machine without audio.cpp", found)
	}
}
