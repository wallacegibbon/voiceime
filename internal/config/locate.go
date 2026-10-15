package config

import (
	"os"
	"path/filepath"
	"strings"
)

// This file answers one question: where is the audio.cpp checkout on this
// machine?
//
// It exists because `audio_cpp_dir` has a default that is only right on the
// machine voiceime was developed on ("~/playground/audio.cpp"), and a deployment
// on any other machine used to have nothing better to offer than the placeholder
// `voiceime config set audio_cpp_dir /path/to/audio.cpp`. Locating the checkout
// turns that into a real command, and lets a fresh install work with no
// configuration at all when the checkout sits in an obvious place.
//
// The candidate list is deliberately short and conventional. Guessing wrong is
// worse than not guessing: a stale checkout of audio.cpp would silently run a
// different build of the CLI and a different model manager than the user
// intended.

// AudioCPPMarkers are the files that make a directory recognisable as an
// audio.cpp checkout. Either one is enough: a checkout that has not been built
// yet still has the model manager, and a build tree left behind by a deleted
// checkout still has the CLI.
const (
	audioCPPManagerMarker = "tools/model_manager_v2.py"
	audioCPPBuildGlob     = "build/*/bin/audiocpp_cli"
)

// IsAudioCPPCheckout reports whether a directory looks like an audio.cpp
// checkout.
func IsAudioCPPCheckout(dir string) bool {
	if dir == "" {
		return false
	}
	if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(audioCPPManagerMarker))); err == nil && !info.IsDir() {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(audioCPPBuildGlob)))
	return len(matches) > 0
}

// LocateAudioCPP returns the first candidate directory that looks like an
// audio.cpp checkout, or "".
func LocateAudioCPP(cfg *Config) string {
	return locateCheckout(checkoutCandidates(cfg))
}

// locateCheckout is the testable half: given candidates in priority order, return
// the first that is a checkout.
func locateCheckout(candidates []string) string {
	for _, candidate := range candidates {
		if IsAudioCPPCheckout(candidate) {
			return candidate
		}
	}
	return ""
}

// checkoutCandidates lists the places a checkout is commonly kept, most specific
// first. The configured directory and any explicitly named binary come first, so a
// deliberate choice always beats a guess; the conventional locations after them
// are what make an unconfigured machine work.
func checkoutCandidates(cfg *Config) []string {
	var candidates []string
	appendUnique := func(path string) {
		if path == "" {
			return
		}
		expanded := Expand(path)
		for _, existing := range candidates {
			if existing == expanded {
				return
			}
		}
		candidates = append(candidates, expanded)
	}

	if cfg != nil {
		appendUnique(cfg.AudioCPPDir)
		appendUnique(checkoutFromBinary(cfg.ASRBin))
	}
	appendUnique(checkoutFromBinary(os.Getenv("AUDIOCPP_CLI")))
	appendUnique(checkoutFromBinary(os.Getenv("VOICEIME_ASR_BIN")))

	home, err := os.UserHomeDir()
	if err == nil {
		for _, relative := range []string{
			"audio.cpp",
			filepath.Join("playground", "audio.cpp"),
			filepath.Join("src", "audio.cpp"),
			filepath.Join("code", "audio.cpp"),
			filepath.Join("git", "audio.cpp"),
			filepath.Join("repos", "audio.cpp"),
			filepath.Join("workspace", "audio.cpp"),
			filepath.Join(".local", "share", "audio.cpp"),
		} {
			appendUnique(filepath.Join(home, relative))
		}
	}
	for _, system := range []string{"/opt/audio.cpp", "/usr/local/src/audio.cpp", "/srv/audio.cpp"} {
		appendUnique(system)
	}
	return candidates
}

// checkoutFromBinary walks up from a binary such as
// <checkout>/build/linux-cpu-release/bin/audiocpp_cli looking for a directory that
// is a checkout.
//
// Four levels is enough for the layouts audio.cpp produces
// (build/<config>/bin/<binary>) and small enough to be free.
func checkoutFromBinary(binary string) string {
	if binary == "" {
		return ""
	}
	dir := filepath.Dir(Expand(binary))
	for range 4 {
		if IsAudioCPPCheckout(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent, string(filepath.Separator)) {
			return ""
		}
		dir = parent
	}
	return ""
}
