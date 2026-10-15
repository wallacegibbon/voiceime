package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A checkout is only recognised by what voiceime needs from it: the model manager
// script, or a built CLI. Nothing here walks the filesystem beyond these
// candidates, because guessing wrong would silently run another checkout's CLI.

func TestIsAudioCPPCheckout(t *testing.T) {
	managerOnly := t.TempDir()
	if err := os.MkdirAll(filepath.Join(managerOnly, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managerOnly, "tools", "model_manager_v2.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	builtOnly := t.TempDir()
	binary := filepath.Join(builtOnly, "build", "linux-cpu-release", "bin")
	if err := os.MkdirAll(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binary, "audiocpp_cli"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		dir  string
		want bool
	}{
		"model manager only": {managerOnly, true},
		"a built CLI only":   {builtOnly, true},
		"an empty directory": {t.TempDir(), false},
		"an empty string":    {"", false},
	}
	for name, test := range cases {
		if got := IsAudioCPPCheckout(test.dir); got != test.want {
			t.Errorf("%s: IsAudioCPPCheckout(%q) = %v, want %v", name, test.dir, got, test.want)
		}
	}
}

func TestCheckoutFromBinaryWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "model_manager_v2.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "build", "linux-cpu-release", "bin")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := checkoutFromBinary(filepath.Join(deep, "audiocpp_cli")); got != root {
		t.Errorf("checkoutFromBinary = %q, want %q", got, root)
	}
	// A binary with no checkout above it must not walk up to something unrelated.
	if got := checkoutFromBinary(filepath.Join(t.TempDir(), "build", "x", "bin", "audiocpp_cli")); got != "" {
		t.Errorf("checkoutFromBinary = %q, want nothing", got)
	}
}

func TestLocateCheckoutPrefersTheFirstRealCandidate(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "tools", "model_manager_v2.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidates := []string{filepath.Join(t.TempDir(), "absent"), "", real, t.TempDir()}
	if got := locateCheckout(candidates); got != real {
		t.Errorf("locateCheckout = %q, want %q", got, real)
	}
	if got := locateCheckout([]string{filepath.Join(t.TempDir(), "absent")}); got != "" {
		t.Errorf("locateCheckout = %q, want nothing when no candidate is a checkout", got)
	}
}

// The configured directory must be tried before the conventional locations, so a
// deliberate choice is never overridden by a guess.
func TestCheckoutCandidatesPutTheConfiguredDirectoryFirst(t *testing.T) {
	cfg := Default()
	cfg.AudioCPPDir = "/somewhere/audio.cpp"
	candidates := checkoutCandidates(cfg)
	if len(candidates) == 0 || candidates[0] != "/somewhere/audio.cpp" {
		t.Fatalf("candidates = %v, want the configured directory first", candidates)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			t.Errorf("candidate %q appears twice", candidate)
		}
		seen[candidate] = true
	}
}
