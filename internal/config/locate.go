package config

import (
	"os"
	"path/filepath"
	"sort"
)

// serverBinaryName is the resident host that serves offline decodes. It lives beside
// audiocpp_cli in the same build tree, which is why one recorded path is enough: the
// settings name the CLI and the server is its sibling.
const serverBinaryName = "audiocpp_server"

// audioCPPBuildGlob is where a built audio.cpp CLI lives inside a checkout.
const audioCPPBuildGlob = "build/*/bin/audiocpp_cli"

// This file answers one question, and answers it once: is there an audio.cpp CLI on this
// machine worth pointing asr_bin at. `voiceime apply` asks it, once, when the settings name
// nothing; nothing else does. Every other command reads the path the settings record, so a
// build tree appearing, disappearing or changing name later cannot change what is running -
// see FindASRBinary.

// DiscoverASRBinaries returns the audio.cpp CLIs found on this machine, most plausible
// first. It returns them all rather than one because a checkout with several build trees
// (a CPU build and a CUDA build, say) is a decision: apply points at the first and names
// the rest, so the caller can choose instead of having it chosen silently.
func DiscoverASRBinaries() []string {
	var found []string
	for _, root := range checkoutCandidates() {
		found = appendUnique(found, buildBinaries(root)...)
	}
	return found
}

// checkoutCandidates are the directories a checkout plausibly lives in. Only the ones that
// hold a built CLI are worth returning, so this list is long on purpose: the cost is a
// handful of stat calls in one command that a human runs once.
func checkoutCandidates() []string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
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
			candidates = append(candidates, filepath.Join(home, relative))
		}
	}
	for _, system := range []string{"/opt/audio.cpp", "/usr/local/src/audio.cpp", "/srv/audio.cpp"} {
		candidates = append(candidates, system)
	}
	return candidates
}

// buildBinaries returns the runnable CLIs one checkout root holds, in a stable order.
func buildBinaries(root string) []string {
	matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(audioCPPBuildGlob)))
	sort.Strings(matches)
	var found []string
	for _, match := range matches {
		if isRunnable(match) {
			found = append(found, match)
		}
	}
	return found
}

func isRunnable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func appendUnique(existing []string, add ...string) []string {
	for _, candidate := range add {
		duplicate := false
		for _, have := range existing {
			if have == candidate {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, candidate)
		}
	}
	return existing
}
