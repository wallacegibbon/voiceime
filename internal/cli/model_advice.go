package cli

import (
	"fmt"

	"github.com/wallacegibbon/voiceime/internal/models"
)

// noModelFix is the fix shared by every "nothing to serve" answer. voiceime does not
// fetch checkpoints, so the runnable command is the one that names a file the caller has
// already put on disk; the placeholder is the same style as the asr_bin and audio_cpp_dir
// fixes.
const noModelFix = "voiceime config set model /path/to/model.gguf"

// modelsDirFix is the fix when the scanned directory itself is not there. That is a
// different problem from an empty one: the answer is a path, not a download.
const modelsDirFix = "voiceime config set models_dir /path/to/models"

// noModelAdvice says what to put there. An answer that only says "no model" leaves a
// caller to guess a family and a size, and the two differ by 20x on disk.
func noModelAdvice(modelsDir string) string {
	recommended := models.RecommendedModels()[0]
	return fmt.Sprintf("nothing to serve: put a GGUF in %s and point model at it"+
		"   # recommended: %s (%s)", modelsDir, recommended.ID, recommended.Reason)
}
