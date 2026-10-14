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

// indexMu serialises read-modify-write of the inspection cache. Two commands can
// run at once (a doctor and a models list, or two agents), and both would
// otherwise load the index, modify their own copy and save, losing the other's
// work - which shows up later as models being re-inspected for no reason.
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

func saveIndex(path string, loaded index) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
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
	for _, item := range found {
		entry, ok := cached.Models[item.path]
		if force || !ok || entry.SizeBytes != item.size || entry.ModifiedUnix != item.modified {
			if progress != nil {
				progress(item.path)
			}
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
		if err := saveIndex(indexPath, cached); err != nil {
			return models, fmt.Errorf("models found but the index could not be cached: %w", err)
		}
	}
	return models, nil
}

// inspect runs `audiocpp_cli --model <path> --inspect` and parses the key=value
// report. The command is cheap (a few hundred milliseconds at most) because it
// reads the checkpoint header rather than the weights.
func inspect(asrBinary, path string) indexEntry {
	ctx, cancel := contextWithTimeout(90 * time.Second)
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

// fastRepairFamilies are the families whose offline decode keeps up with speech
// on a CPU, so a repair of a finished segment finishes while the user is still
// talking. The classification is measured, not guessed: on the reference machine
// a 4.2s segment costs about 0.55s with SenseVoice (roughly 8x realtime including
// model load) and about 5.7s with Qwen3-ASR-0.6B (about 0.75x realtime), and the
// larger LLM-based ASR families are slower still. See docs/measurements.md.
var fastRepairFamilies = map[string]bool{
	"sense_asr":     true,
	"moonshine_asr": true,
	"kroko_asr":     true,
	"parakeet_tdt":  true,
	"citrinet_asr":  true,
	"canary_asr":    true,
	"gigaam_asr":    true,
	"moonshine":     true,
	"whistle_asr":   true,
	"vibeasr":       true,
	"niagara_asr":   true,
	"hviske_asr":    true,
	"hviske_asr_v6": true,
	"owsm":          true,
	"owsm_ctc":      true,
	"granite5asr":   true,
	"sensevoice":    true,
}

// RepairIsRealtime reports whether re-decoding a finished segment with this model
// is expected to keep up with speech on a CPU-only machine.
func RepairIsRealtime(model Model) bool { return fastRepairFamilies[model.Family] }

// SuggestRepair returns the most accurate installed ASR model that is *different*
// from the one already used for streaming, or nil when there is nothing extra to
// suggest.
//
// It is only used for advice, never to silently select a model: a stronger repair
// model is a real trade (better text against a repair that lags behind speech),
// and the user is the one who knows whether that trade is worth it on their
// hardware.
func SuggestRepair(scanned []Model, streamingFamily string) *Model {
	var best *Model
	bestScore := -1
	for i := range scanned {
		model := &scanned[i]
		if model.Error != "" || model.Family == streamingFamily {
			continue
		}
		if !model.Supports("asr", "offline") || RepairIsRealtime(*model) {
			continue
		}
		// Prefer larger checkpoints within the accurate set: among LLM-based ASR
		// models, size is what buys accuracy.
		score := int(model.SizeBytes >> 20)
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
