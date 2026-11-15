package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ASRServerName is the audio.cpp process voiceime runs: the resident server. The CLI in the
// same directory is not used at all - inference goes over HTTP - which is why one path, the
// one the service unit names, is the whole answer to "which audio.cpp".
const ASRServerName = "audiocpp_server"

// audioCPPBuildGlob is where a built audio.cpp server lives inside a checkout.
const audioCPPBuildGlob = "build/*/bin/" + ASRServerName

// ASRServer returns the audio.cpp server to run, and where the answer came from. It is a
// deploy-time question: `voiceime apply` asks it once and writes the result into the service
// unit, so nothing at runtime has to look for anything.
//
// A built checkout comes first, then PATH. A checkout is the more specific answer: on a
// machine that has both, the copy installed into ~/.local/bin is usually an older one left
// there by hand (measured on the reference machine: 34 MB against the build tree's 865 MB,
// both dated the same day, different md5s).
func ASRServer() (path, source string, err error) {
	if found := DiscoverASRServers(); len(found) > 0 {
		return found[0], "a built audio.cpp checkout", nil
	}
	if found, lookupErr := executableFromPath(ASRServerName); lookupErr == nil {
		return found, "PATH", nil
	}
	return "", "", fmt.Errorf(
		"no audio.cpp server: build audio.cpp (%s), or install %s somewhere on PATH",
		"cmake -B build && cmake --build build", ASRServerName)
}

// DiscoverASRServers returns the audio.cpp servers built on this machine, in a stable order,
// so that a machine holding several build trees is a fact `voiceime apply` reports instead of
// one it picks silently.
func DiscoverASRServers() []string {
	var found []string
	for _, root := range checkoutCandidates() {
		found = appendUnique(found, buildBinaries(root)...)
	}
	return found
}

// checkoutCandidates are the directories a checkout plausibly lives in. Only the ones that
// hold a built server are worth returning, so this list is long on purpose: the cost is a
// handful of stat calls in the one command that installs.
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

// buildBinaries returns the runnable audio.cpp servers one checkout root holds, in a stable
// order.
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

func executableFromPath(name string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if isRunnable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s is not on PATH", name)
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
