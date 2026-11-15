package cli

import (
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/model"
)

// noModelFix is the fix shared by every "no checkpoint named" answer. voiceime fetches
// nothing and scans nothing: the runnable command is the one that names a file the caller has
// already put on disk.
const noModelFix = "voiceime config set model /path/to/model.gguf"

// recommendedCheckpoints is what to fetch. This table exists because every "nothing named"
// answer has to say what would fix it, and an empty answer would leave a caller to guess a
// family and a size - the two differ by 20x on disk.
//
// Only what can be checked from the outside is claimed: size on disk and which languages are
// accepted. A speed figure in an error message is a number nobody can verify; the measured
// ones live in docs/measurements.md with the machine they were measured on.
var recommendedCheckpoints = []struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}{
	{"sensevoice_small_q8", "242 MiB; reads a whole recording in one pass; zh, en, yue and more - the one the reference machine uses"},
	{"qwen3_asr_0_6b_q8_0", "1.07 GiB; the larger checkpoint, which also reads zh and en"},
	{"kroko_asr_community_q8_0", "160 MiB, one language per package (de/en/es/fr/it/he/nl/pt/sv/tr)"},
	{"moonshine_streaming_tiny_q8_0", "58 MiB, English-only, useful for testing"},
}

// noModelAdvice says what to name. The first recommendation is named because the choice is
// made once and its cost is the thing a reader needs to decide with.
func noModelAdvice() string {
	recommended := recommendedCheckpoints[0]
	return fmt.Sprintf("no checkpoint is named: fetch a GGUF and point model at it"+
		"   # for example %s (%s)", recommended.ID, recommended.Reason)
}

// describeCheckpoint is the one-line answer to "which model am I using", shared by the
// status output and the doctor finding so they cannot disagree.
func describeCheckpoint(info model.Info) string {
	tasks := ""
	if info.Says() {
		tasks = " " + strings.Join(info.Tasks, "/")
	}
	return fmt.Sprintf("%s (%s, %s)%s", info.Path, info.Family, model.Sizes(info.SizeBytes), tasks)
}
