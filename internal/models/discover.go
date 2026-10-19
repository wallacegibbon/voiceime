// Package models discovers the model packages installed on this machine and
// downloads new ones through audio.cpp's own model manager.
//
// Discovery works by asking audio.cpp, never by guessing from file names: the
// CLI's `--inspect` identifies a model from its contents and prints its family,
// tasks, modes and languages. That answer is cached against the file's size and
// mtime, so a rescan costs one stat per file and the "refresh" the user asked
// for becomes cheap enough to be unnecessary in practice - though
// `voiceime models refresh` still forces it, because an agent should not have to
// know that.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Task is one entry of the `task=<name> modes=<a,b>` lines audio.cpp prints.
type Task struct {
	Name  string   `json:"name"`
	Modes []string `json:"modes"`
}

// Model is what audio.cpp reports about one model directory or GGUF file.
type Model struct {
	Path               string    `json:"path"`
	Family             string    `json:"family"`
	Variant            string    `json:"variant"`
	Tasks              []Task    `json:"tasks"`
	Languages          []string  `json:"languages"`
	SupportsTimestamps bool      `json:"supports_timestamps"`
	SizeBytes          int64     `json:"size_bytes"`
	ModifiedUnix       int64     `json:"modified_unix"`
	InspectedAt        time.Time `json:"inspected_at"`
	// Error records a failed inspection (a file that is not a model, or a model
	// this build cannot load). Such entries are kept so the UI-less surface can
	// explain the directory instead of silently ignoring files.
	Error string `json:"error,omitempty"`
}

// Supports reports whether the model advertises a task in a mode.
func (m Model) Supports(task, mode string) bool {
	for _, candidate := range m.Tasks {
		if candidate.Name != task {
			continue
		}
		for _, candidateMode := range candidate.Modes {
			if candidateMode == mode {
				return true
			}
		}
	}
	return false
}

// StreamingASR reports whether the model can drive live dictation.
func (m Model) StreamingASR() bool { return m.Supports("asr", "streaming") }

// SupportsLanguage reports whether the model lists a language, treating an empty
// list as "unknown" rather than "no".
func (m Model) SupportsLanguage(language string) bool {
	if len(m.Languages) == 0 || language == "" {
		return true
	}
	for _, candidate := range m.Languages {
		if strings.EqualFold(candidate, language) {
			return true
		}
	}
	return false
}

// indexEntry is the cache record for one path. Size and mtime are the cache key:
// a re-downloaded or replaced model is re-inspected, an untouched one is not.
type indexEntry struct {
	SizeBytes    int64    `json:"size_bytes"`
	ModifiedUnix int64    `json:"modified_unix"`
	Family       string   `json:"family"`
	Variant      string   `json:"variant"`
	Tasks        []Task   `json:"tasks"`
	Languages    []string `json:"languages"`
	Timestamps   bool     `json:"supports_timestamps"`
	Error        string   `json:"error,omitempty"`
}

type index struct {
	Version int                   `json:"version"`
	Models  map[string]indexEntry `json:"models"`
}

const indexVersion = 1

// indexMu serialises the read-modify-write of the inspection cache inside this
// process, so two goroutines cannot each load the index, modify their own copy and
// save, losing the other's work. It does not reach across processes - two commands
// running at once are separate processes - so the cross-process half is done by the
// writer: config.WriteFile makes the file appear whole or not at all, and saveIndex
// merges instead of overwriting (see its comment).
var indexMu sync.Mutex

func loadIndex(path string) index {
	loaded := index{Version: indexVersion, Models: map[string]indexEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return loaded
	}
	if err := json.Unmarshal(raw, &loaded); err != nil || loaded.Models == nil {
		return index{Version: indexVersion, Models: map[string]indexEntry{}}
	}
	return loaded
}

// saveIndex writes what this scan learned, taking every entry it did not inspect from
// whatever is on disk at that moment.
//
// Two commands can scan at the same time - a doctor and a models list, or two agents - and
// they are separate processes, so the mutex above cannot serialise them. Writing this
// process's whole snapshot over the file lost the other's work entirely: the loser's
// freshly inspected entries vanished and the next scan paid for them again, which on this
// machine is about six seconds per model (measured; `audiocpp_cli --inspect` is not the
// few hundred milliseconds the comment below the inspector assumes). Merging narrows the
// window from the whole scan to the microseconds between this read and the rename, and
// every entry this process did not inspect is taken from disk - so the answer that was
// already there, possibly newer, is kept.
//
// inspected is the set of paths this run actually re-inspected; those are the entries that
// may be newer than the file's, and nothing else is.
func saveIndex(path string, mine index, inspected map[string]bool) error {
	if onDisk := loadIndex(path); len(onDisk.Models) > 0 {
		for key := range mine.Models {
			if inspected[key] {
				continue
			}
			if kept, found := onDisk.Models[key]; found {
				mine.Models[key] = kept
			}
		}
	}
	raw, err := json.MarshalIndent(mine, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFile(path, raw, 0o644)
}

// candidate is something in the models directory worth asking about.
type candidate struct {
	path     string
	size     int64
	modified int64
}

// candidates lists GGUF files and safetensors-style directories under dir.
// Directory layouts are included because audio.cpp loads those directly (a
// HuggingFace checkpoint directory is a first-class model path).
func candidates(dir string) []candidate {
	var found []candidate
	walk := func(path string) {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if info.IsDir() {
			// A directory is a model only if it looks like a checkpoint root;
			// otherwise it is a package directory holding a GGUF inside it.
			if _, err := os.Stat(filepath.Join(path, "config.json")); err != nil {
				return
			}
		}
		found = append(found, candidate{path: path, size: info.Size(), modified: info.ModTime().Unix()})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if nested, err := os.ReadDir(full); err == nil {
				for _, inner := range nested {
					if strings.HasSuffix(inner.Name(), ".gguf") {
						walk(filepath.Join(full, inner.Name()))
					}
				}
			}
			walk(full) // possibly a checkpoint root
			continue
		}
		if strings.HasSuffix(entry.Name(), ".gguf") {
			walk(full)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	return found
}

// Scan returns every model under the configured directory, inspecting only what
// changed. progress, when non-nil, is called with one line per inspection so a
// long first scan can be narrated.
func Scan(cfg *config.Config, force bool, progress func(string)) ([]Model, error) {
	dir := cfg.ResolvedModelsDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("models directory %s does not exist", dir)
	}
	asrBinary, err := cfg.FindASRBinary()
	if err != nil {
		return nil, err
	}

	indexMu.Lock()
	defer indexMu.Unlock()

	indexPath := config.ModelIndexPath()
	cached := loadIndex(indexPath)
	found := candidates(dir)

	models := make([]Model, 0, len(found))
	dirty := false
	inspected := map[string]bool{}
	for _, item := range found {
		entry, ok := cached.Models[item.path]
		if force || !ok || entry.SizeBytes != item.size || entry.ModifiedUnix != item.modified {
			if progress != nil {
				progress(item.path)
			}
			inspected[item.path] = true
			entry = inspect(asrBinary, item.path)
			entry.SizeBytes = item.size
			entry.ModifiedUnix = item.modified
			cached.Models[item.path] = entry
			dirty = true
		}
		models = append(models, Model{
			Path:               item.path,
			Family:             entry.Family,
			Variant:            entry.Variant,
			Tasks:              entry.Tasks,
			Languages:          entry.Languages,
			SupportsTimestamps: entry.Timestamps,
			SizeBytes:          item.size,
			ModifiedUnix:       item.modified,
			InspectedAt:        time.Now(),
			Error:              entry.Error,
		})
	}
	// Drop cache records for files that are gone, so a deleted model does not
	// come back from the index forever.
	alive := map[string]bool{}
	for _, item := range found {
		alive[item.path] = true
	}
	for path := range cached.Models {
		if !alive[path] {
			delete(cached.Models, path)
			dirty = true
		}
	}
	if dirty {
		if err := saveIndex(indexPath, cached, inspected); err != nil {
			return models, fmt.Errorf("models found but the index could not be cached: %w", err)
		}
	}
	return models, nil
}

// inspect runs `audiocpp_cli --model <path> --inspect` and parses the key=value
// report. It is meant to be cheap - it reads the checkpoint header rather than the
// weights - but on the reference machine it measured 5.95s for one 1 GiB checkpoint, so
// the cache is worth more than the comment here used to assume, and so is not losing an
// entry to a concurrent writer (see saveIndex).
func inspect(asrBinary, path string) indexEntry {
	// The only bound model discovery puts on a subprocess: --inspect reads a checkpoint
	// header, and a tool that has not answered in 90s is wedged.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, asrBinary, "--model", path, "--inspect").CombinedOutput()
	text := string(output)
	if err != nil && text == "" {
		return indexEntry{Error: err.Error()}
	}
	entry := parseInspect(text)
	if entry.Family == "" {
		// audio.cpp explains itself on stderr ("no registered model loader can
		// inspect: ..."), which is more useful than a bare exit status.
		entry.Error = strings.TrimSpace(lastNonEmptyLine(text))
		if entry.Error == "" && err != nil {
			entry.Error = err.Error()
		}
	}
	return entry
}

// parseInspect reads the key=value lines. Only the fields voiceime acts on are
// kept; the report contains more (graph arena sizes, weight listings) and
// duplicating all of it here would mean tracking upstream changes for no gain.
func parseInspect(text string) indexEntry {
	entry := indexEntry{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "family":
			entry.Family = value
		case "variant":
			entry.Variant = value
		case "task":
			// Format: "task=asr modes=offline,streaming"
			fields := strings.Fields(value)
			if len(fields) == 0 {
				continue
			}
			task := Task{Name: fields[0]}
			for _, field := range fields[1:] {
				if modes, found := strings.CutPrefix(field, "modes="); found {
					task.Modes = strings.Split(modes, ",")
				}
			}
			entry.Tasks = append(entry.Tasks, task)
		case "languages":
			if value != "" {
				entry.Languages = strings.Split(value, ",")
			}
		case "supports_timestamps":
			entry.Timestamps = value == "true"
		}
	}
	return entry
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// PickStreaming returns the model voiceime should use for live dictation when
// the user has not chosen one.
//
// The ranking is a measured preference, not a guess: on CPU, SenseVoice-Small is
// the only small multilingual streaming model here that keeps up comfortably
// (about 20x realtime on an 8-core laptop), Qwen3-ASR-0.6B is roughly 3.5x
// realtime and therefore better used as the repair model, and the 1.7B streaming
// models do not run in real time at all on a CPU. See docs/measurements.md.
func PickStreaming(models []Model, language string) *Model {
	ranking := map[string]int{
		"sense_asr":        100,
		"qwen3_asr":        90,
		"parakeet_tdt":     80,
		"kroko_asr":        70,
		"moonshine_asr":    60,
		"higgs_audio_stt":  50,
		"nemotron_asr":     40,
		"voxtral_realtime": 30,
	}
	var best *Model
	bestScore := -1
	for i := range models {
		model := &models[i]
		if model.Error != "" || !model.StreamingASR() {
			continue
		}
		score := ranking[model.Family]
		if score == 0 {
			score = 10
		}
		if model.SupportsLanguage(language) {
			score += 20
		}
		if score > bestScore {
			best, bestScore = model, score
		}
	}
	return best
}

// ASRModels filters a scan down to the models voiceime can actually use, which
// is what every caller except an explicit "show me everything" wants.
//
// This matters more than it looks: the models directory is shared with whatever
// else the user keeps there - text LLMs, embedding models, vision projectors in
// the reference machine's case. Listing those as unusable models would bury the
// two that matter and make an agent think the install is broken.
func ASRModels(scanned []Model) []Model {
	var usable []Model
	for _, model := range scanned {
		if model.Error != "" {
			continue
		}
		if model.Supports("asr", "offline") || model.Supports("asr", "streaming") {
			usable = append(usable, model)
		}
	}
	return usable
}

// Lookup finds a model by path among the scanned set.
func Lookup(models []Model, path string) *Model {
	abs, err := filepath.Abs(config.Expand(path))
	if err != nil {
		abs = config.Expand(path)
	}
	for i := range models {
		if models[i].Path == abs {
			return &models[i]
		}
	}
	return nil
}

// Sizes formats a byte count for humans.
func Sizes(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 2, 64) + " GiB"
	case bytes >= 1<<20:
		return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64) + " MiB"
	case bytes >= 1<<10:
		return strconv.FormatFloat(float64(bytes)/(1<<10), 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatInt(bytes, 10) + " B"
	}
}
