package config

import (
	"os"
	"path/filepath"
	"strings"
)

// This file answers one question: where is the audio.cpp checkout on this

const (
	audioCPPManagerMarker = "tools/model_manager_v2.py"
	audioCPPBuildGlob     = "build/*/bin/audiocpp_cli"
	// serverBinaryName is the resident host that serves offline decodes. It lives
	// beside audiocpp_cli in the same build tree.
	serverBinaryName = "audiocpp_server"
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
