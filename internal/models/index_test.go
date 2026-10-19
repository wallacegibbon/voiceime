package models

import (
	"os"
	"path/filepath"
	"testing"
)

// indexAt writes an index file and returns its path, for the merge tests.
func indexAt(t *testing.T, entries map[string]indexEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model_index.json")
	loaded := index{Version: indexVersion, Models: entries}
	if err := saveIndex(path, loaded, map[string]bool{}); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	return path
}

// A scan must not erase what another process just learned.
//
// Two commands scan at the same time (a doctor and a models list, two agents), and they are
// separate processes, so the package mutex cannot serialise them. Writing this process's
// whole snapshot over the file lost the other's work: on this machine one `--inspect` costs
// 5.95s, so the loss is paid again on the next scan, visibly. The writer therefore takes
// every entry it did not inspect from disk.
func TestSaveIndexKeepsAnotherProcessesEntries(t *testing.T) {
	// What the other process wrote while this one was still inspecting.
	path := indexAt(t, map[string]indexEntry{
		"/models/other.gguf": {SizeBytes: 10, ModifiedUnix: 1, Family: "sense_asr"},
	})

	// This process inspected nothing of its own - it was seeded before that write - and is
	// about to save its (older) snapshot.
	mine := index{Version: indexVersion, Models: map[string]indexEntry{
		"/models/mine.gguf":  {SizeBytes: 20, ModifiedUnix: 2, Family: "qwen3_asr"},
		"/models/other.gguf": {SizeBytes: 10, ModifiedUnix: 1, Family: ""}, // stale copy
	}}
	if err := saveIndex(path, mine, map[string]bool{"/models/mine.gguf": true}); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}

	merged := loadIndex(path)
	if got := merged.Models["/models/mine.gguf"].Family; got != "qwen3_asr" {
		t.Errorf("my own inspected entry = %q, want it kept", got)
	}
	if got := merged.Models["/models/other.gguf"].Family; got != "sense_asr" {
		t.Errorf("the other process's entry = %q, want the one from disk", got)
	}
	if len(merged.Models) != 2 {
		t.Errorf("index holds %d entries, want 2", len(merged.Models))
	}
}

// What this run inspected wins over an older copy on disk, and entries for files that are
// gone are not resurrected by the merge.
func TestSaveIndexPrefersWhatThisRunInspected(t *testing.T) {
	path := indexAt(t, map[string]indexEntry{
		"/models/a.gguf":    {SizeBytes: 1, ModifiedUnix: 1, Family: "old"},
		"/models/gone.gguf": {SizeBytes: 2, ModifiedUnix: 2, Family: "sense_asr"},
	})

	// One file was replaced (so it was inspected again) and one was deleted, so it is
	// absent from mine - which is how Scan prunes.
	mine := index{Version: indexVersion, Models: map[string]indexEntry{
		"/models/a.gguf": {SizeBytes: 9, ModifiedUnix: 9, Family: "new"},
	}}
	if err := saveIndex(path, mine, map[string]bool{"/models/a.gguf": true}); err != nil {
		t.Fatal(err)
	}

	loaded := loadIndex(path)
	if got := loaded.Models["/models/a.gguf"].Family; got != "new" {
		t.Errorf("inspected entry = %q, want the result of this run", got)
	}
	if _, found := loaded.Models["/models/gone.gguf"]; found {
		t.Error("a deleted model came back from the disk copy")
	}
}

// A file written by a future version is not reused as if it were this one's: the version
// field exists so a format change can invalidate the cache, and a merge must not defeat it.
func TestSaveIndexWritesTheCurrentVersion(t *testing.T) {
	path := indexAt(t, map[string]indexEntry{})
	if err := os.WriteFile(path, []byte(`{"version":99,"models":{"/x.gguf":{"family":"future"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := index{Version: indexVersion, Models: map[string]indexEntry{
		"/models/mine.gguf": {Family: "qwen3_asr"},
	}}
	if err := saveIndex(path, mine, map[string]bool{"/models/mine.gguf": true}); err != nil {
		t.Fatal(err)
	}

	loaded := loadIndex(path)
	if loaded.Version != indexVersion {
		t.Errorf("version = %d, want %d", loaded.Version, indexVersion)
	}
	// The merge only fills in entries whose path this scan already knows: an entry written
	// under another version's schema is not carried into this one.
	if _, found := loaded.Models["/x.gguf"]; found {
		t.Error("an entry in a foreign format was carried into the current index")
	}
	if _, found := loaded.Models["/models/mine.gguf"]; !found {
		t.Error("this run's own entry is missing")
	}
}
