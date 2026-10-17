package models

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
// code path upstream tests against HuggingFace. A second implementation here
// would drift from the registry without anyone noticing. voiceime's contribution
// is the *policy*: which endpoint to use, which packages to recommend, and a
// machine-readable result an agent can act on.
type Manager struct {
	Script    string
	ModelsDir string
	// Endpoint is the HF-protocol host downloads go through, passed to the
	// script as HF_ENDPOINT.
	Endpoint string
	Python   string
}

// ErrBadEndpoint marks a download_mirror that is not a usable endpoint URL, so a
// caller can offer the fix for that setting rather than for the surrounding
// tooling.
var ErrBadEndpoint = errors.New("download_mirror")

// NewManager resolves the script, the interpreter to run it with and the
// endpoint to download from.
func NewManager(cfg *config.Config) (*Manager, error) {
	script, err := cfg.FindModelManager()
	if err != nil {
		return nil, err
	}
	python := config.FindExecutable("python3", "python")
	if python == "" {
		return nil, fmt.Errorf("python3 is required to run %s but was not found on PATH", script)
	}
	endpoint, err := cfg.DownloadEndpoint()
	if err != nil {
		return nil, fmt.Errorf("%w %v", ErrBadEndpoint, err)
	}
	return &Manager{
		Script:    script,
		ModelsDir: cfg.ResolvedModelsDir(),
		Endpoint:  endpoint,
		Python:    python,
	}, nil
}

// environment returns the extra environment the script needs. The endpoint is
// always set explicitly rather than inherited: HF_ENDPOINT is the variable
// huggingface_hub reads, so an ambient value would otherwise decide where
// downloads come from and voiceime's configuration would stop being the single
// source of truth for it.
func (m *Manager) environment() []string {
	return []string{"HF_ENDPOINT=" + m.Endpoint}
}

// command builds an exec.Cmd with the endpoint environment applied.
func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{m.Script}, args...)
	cmd := exec.CommandContext(ctx, m.Python, full...)
	cmd.Env = append(cmd.Environ(), m.environment()...)
	return cmd
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
// script explains most failures (404 on a mirror, repo renamed upstream) in its
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
// the reasons are measured on a CPU-only machine; see docs/measurements.md and
// DEPLOY.md#4-install-a-model for the CPU/GPU split the first two entries encode.
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
		{"sensevoice_small_q8", "242 MiB; ~21x realtime streaming on CPU and 0.55s to repair a finished sentence - the streaming model on every machine, CPU or GPU"},
		{"qwen3_asr_0_6b_q8_0", "1.07 GiB; the more accurate offline reader. Add it as refine_model only with a GPU backend: on CPU one repair costs ~5.7s for a 4.2s sentence"},
		{"kroko_asr_community_q8_0", "160 MiB, native streaming, one language per package (de/en/es/fr/it/he/nl/pt/sv/tr)"},
		{"moonshine_streaming_tiny_q8_0", "58 MiB, English-only streaming, useful for testing latency"},
	}
}
