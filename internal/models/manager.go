package models

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Manager drives audio.cpp's model manager script.
//
// Delegating the download to that script is deliberate: it owns the package
// registry, the pinned revisions per package and the mirror rules, and it is the
// code path upstream tests against both HuggingFace and ModelScope. A second
// implementation here would drift from the registry without anyone noticing.
// voiceime's contribution is the *policy*: which mirror to use, which packages to
// recommend, and a machine-readable result an agent can act on.
type Manager struct {
	Script    string
	ModelsDir string
	Mirror    string // hf-mirror, huggingface or modelscope
	Python    string
}

// NewManager resolves the script and the interpreter to run it with.
func NewManager(cfg *config.Config) (*Manager, error) {
	script, err := cfg.FindModelManager()
	if err != nil {
		return nil, err
	}
	python := config.FindExecutable("python3", "python")
	if python == "" {
		return nil, fmt.Errorf("python3 is required to run %s but was not found on PATH", script)
	}
	return &Manager{
		Script:    script,
		ModelsDir: cfg.ResolvedModelsDir(),
		Mirror:    cfg.DownloadMirror,
		Python:    python,
	}, nil
}

// Package is one installable package id.
type Package struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	Format    string `json:"format"`
	Precision string `json:"precision"`
	Default   bool   `json:"default"`
}

// Status is the per-package state reported by `sizes` and `installed`.
type Status struct {
	ID             string `json:"id"`
	SizeBytes      *int64 `json:"size_bytes"`
	State          string `json:"state"`
	Message        string `json:"message"`
	Installed      bool   `json:"installed"`
	VersionState   string `json:"version_state"`
	LocalRevision  string `json:"local_revision"`
	RemoteRevision string `json:"remote_revision"`
}

// environment returns the extra environment the script needs. The mirror is
// applied through HF_ENDPOINT, which is the same variable huggingface_hub reads,
// so a mirror configured here also applies to anything else the user runs.
func (m *Manager) environment() []string {
	environment := []string{}
	switch m.Mirror {
	case "hf-mirror":
		environment = append(environment, "HF_ENDPOINT=https://hf-mirror.com")
	case "huggingface":
		// Explicitly clear it: the user may have one set globally.
		environment = append(environment, "HF_ENDPOINT=")
	}
	return environment
}

// sourceArgs maps the mirror setting onto the script's own source selection.
// ModelScope is a different transport rather than a different endpoint, so it
// takes an argument instead of an environment variable.
func (m *Manager) sourceArgs() []string {
	if m.Mirror == "modelscope" {
		return []string{"--source", "modelscope"}
	}
	return nil
}

// command builds an exec.Cmd with the environment and source overrides applied.
func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{m.Script}, args...)
	cmd := exec.CommandContext(ctx, m.Python, full...)
	cmd.Env = append(cmd.Environ(), m.environment()...)
	return cmd
}

// List returns every package the registry knows about.
func (m *Manager) List(ctx context.Context) ([]Package, error) {
	output, err := m.command(ctx, "list", "--json").Output()
	if err != nil {
		return nil, describeExecError("model_manager list", err)
	}
	var packages []Package
	if err := json.Unmarshal(output, &packages); err != nil {
		return nil, fmt.Errorf("model_manager list returned unexpected output: %w", err)
	}
	return packages, nil
}

// Search filters List by a substring of the package id or family.
func (m *Manager) Search(ctx context.Context, needle string) ([]Package, error) {
	packages, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	if needle == "" {
		return packages, nil
	}
	needle = strings.ToLower(needle)
	var matches []Package
	for _, pkg := range packages {
		if strings.Contains(strings.ToLower(pkg.ID), needle) ||
			strings.Contains(strings.ToLower(pkg.Family), needle) {
			matches = append(matches, pkg)
		}
	}
	return matches, nil
}

// Sizes checks what the named packages would download (bytes and reachability)
// without fetching them. Network access is required.
func (m *Manager) Sizes(ctx context.Context, packages ...string) ([]Status, error) {
	args := append([]string{"sizes", "--models-root", m.ModelsDir}, m.sourceArgs()...)
	args = append(args, packages...)
	output, err := m.command(ctx, args...).Output()
	if err != nil {
		return nil, describeExecError("model_manager sizes", err)
	}
	var statuses []Status
	if err := json.Unmarshal(output, &statuses); err != nil {
		return nil, fmt.Errorf("model_manager sizes returned unexpected output: %w", err)
	}
	return statuses, nil
}

// Installed reports which packages are present locally. It makes no network
// calls, so it is safe to run on a machine with no connectivity.
func (m *Manager) Installed(ctx context.Context) ([]Status, error) {
	output, err := m.command(ctx, "installed", "--models-root", m.ModelsDir).Output()
	if err != nil {
		return nil, describeExecError("model_manager installed", err)
	}
	var statuses []Status
	if err := json.Unmarshal(output, &statuses); err != nil {
		return nil, fmt.Errorf("model_manager installed returned unexpected output: %w", err)
	}
	return statuses, nil
}

// Progress is one event from an install: either a parsed progress record or a
// line of human-readable output. Streaming this rather than buffering means a
// caller can show a progress bar, and an agent can log a long download.
type Progress struct {
	// Phase is filled from AUDIOCPP_PROGRESS records ("download", "verify", ...).
	Phase string `json:"phase,omitempty"`
	// File, Bytes, TotalBytes are filled when the script reports them.
	File       string `json:"file,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	// Line is set for ordinary output lines.
	Line string `json:"line,omitempty"`
}

// Install downloads a package. The callback is called for every progress event
// and may be nil.
func (m *Manager) Install(ctx context.Context, pkg string, progress func(Progress)) error {
	args := []string{"install", pkg, "--models-root", m.ModelsDir, "--progress"}
	args = append(args, m.sourceArgs()...)
	cmd := m.command(ctx, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// The script writes human-readable narration to stderr and progress records
	// to stdout; both are wanted, so they are consumed concurrently.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return describeExecError("model_manager install", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		consume(stderr, nil, progress)
	}()
	consume(stdout, nil, progress)
	<-done

	if err := cmd.Wait(); err != nil {
		return describeExecError("model_manager install "+pkg, err)
	}
	return nil
}

// Uninstall removes only the files belonging to one package.
func (m *Manager) Uninstall(ctx context.Context, pkg string) error {
	output, err := m.command(ctx, "uninstall", pkg, "--models-root", m.ModelsDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("model_manager uninstall %s: %v: %s", pkg, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// consume parses one stream. AUDIOCPP_PROGRESS lines are JSON records the script
// emits for machines; everything else is passed through as narration.
func consume(reader io.Reader, sink io.Writer, progress func(Progress)) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if sink != nil {
			fmt.Fprintln(sink, line)
		}
		if progress == nil {
			continue
		}
		if payload, found := strings.CutPrefix(line, "AUDIOCPP_PROGRESS"); found {
			var record Progress
			if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &record); err == nil {
				progress(record)
				continue
			}
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			progress(Progress{Line: trimmed})
		}
	}
}

// describeExecError adds the failing command's output to the error, because the
// script explains most failures (404 on a mirror, repo not on ModelScope) in its
// output rather than in its exit status.
func describeExecError(what string, err error) error {
	if exit, ok := err.(*exec.ExitError); ok {
		output := strings.TrimSpace(string(exit.Stderr))
		if output == "" {
			output = strings.TrimSpace(exit.Error())
		}
		return fmt.Errorf("%s failed: %s", what, output)
	}
	return fmt.Errorf("%s failed: %w", what, err)
}

// RecommendedPackages is the short list voiceime suggests to a user who has no
// model yet, in the order it suggests them. The ids are real registry ids, and
// the reasons are measured on a CPU-only machine; see docs/measurements.md.
//
// Note what is absent: Audio8-ASR is not offered even though it looks ideal on
// paper, because its spec declares the download as unsupported (the checkpoint
// is CC-BY-NC and audio.cpp ships no public GGUF) and it has no streaming mode.
func RecommendedPackages() []struct {
	ID     string
	Reason string
} {
	return []struct {
		ID     string
		Reason string
	}{
		{"sensevoice_small_q8", "254 MiB, multilingual streaming, ~20x realtime on CPU - the default choice"},
		{"qwen3_asr_0_6b_q8_0", "1.1 GiB, best offline accuracy and punctuation measured here - use as the repair model"},
		{"kroko_asr_community_q8_0", "160 MiB, native streaming, one language per package (de/en/es/fr/it/he/nl/pt/sv/tr)"},
		{"moonshine_streaming_tiny_q8_0", "58 MiB, English-only streaming, useful for testing latency"},
	}
}
