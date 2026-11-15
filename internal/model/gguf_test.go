package model

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writer builds the smallest thing a real GGUF writer produces: a header, a key-value
// section, and the tensors after it, which are never read.
type writer struct {
	fields []byte
	count  uint64
}

func (w *writer) u32(v uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], v)
	w.fields = append(w.fields, raw[:]...)
}

func (w *writer) str(s string) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
	w.fields = append(w.fields, length[:]...)
	w.fields = append(w.fields, s...)
}

func (w *writer) kv(key string, kind uint32, value string) {
	w.str(key)
	w.u32(kind)
	w.str(value)
	w.count++
}

func (w *writer) stringArray(key string, values []string) {
	w.str(key)
	w.u32(9) // array
	w.u32(8) // of strings
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(values)))
	w.fields = append(w.fields, count[:]...)
	for _, value := range values {
		w.str(value)
	}
	w.count++
}

func (w *writer) scalar(key string, kind uint32, width int) {
	w.str(key)
	w.u32(kind)
	w.fields = append(w.fields, make([]byte, width)...)
	w.count++
}

func (w *writer) file(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	raw := append([]byte(magic), 3, 0, 0, 0)
	var counts [16]byte
	binary.LittleEndian.PutUint64(counts[:8], 7) // tensor count
	binary.LittleEndian.PutUint64(counts[8:], w.count)
	raw = append(raw, counts[:]...)
	raw = append(raw, w.fields...)
	raw = append(raw, make([]byte, 64)...) // the tensor data, unread
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The family is the one fact the server entry needs that the path does not carry.
func TestOpenReadsTheFamilyTheServerNeeds(t *testing.T) {
	w := &writer{}
	w.kv("general.architecture", 8, "sensevoice")
	w.scalar("sv.input_size", 4, 4)
	w.stringArray("sv.vocab", []string{"a", "b", "c"})
	w.scalar("sv.blank_id", 5, 4)
	w.kv(familyKey, 8, "sense_asr")
	w.kv("audiocpp.model_spec.version", 8, "1")

	info, err := Open(w.file(t))
	if err != nil {
		t.Fatal(err)
	}
	if info.Family != "sense_asr" {
		t.Errorf("Family = %q, want sense_asr", info.Family)
	}
	if info.Architecture != "sensevoice" {
		t.Errorf("Architecture = %q, want sensevoice", info.Architecture)
	}
	if info.SizeBytes == 0 {
		t.Error("SizeBytes = 0, want the file's size")
	}
}

// A vocabulary sits between the header and the family key in a real file, and it is larger
// than the rest of the metadata: skipping it correctly is the whole job.
func TestOpenSkipsWhatItDoesNotRead(t *testing.T) {
	vocabulary := make([]string, 0, 25000)
	for i := 0; i < 25000; i++ {
		vocabulary = append(vocabulary, "token-"+strings.Repeat("x", 8)+string(rune('a'+i%26)))
	}
	w := &writer{}
	w.kv("general.architecture", 8, "qwen3")
	w.stringArray("tokenizer.ggml.tokens", vocabulary)
	for _, kind := range []struct {
		b     uint32
		width int
	}{{0, 1}, {1, 1}, {2, 2}, {3, 2}, {4, 4}, {5, 4}, {6, 4}, {7, 1}, {10, 8}, {11, 8}, {12, 8}} {
		w.scalar("qwen3.filler", kind.b, kind.width)
	}
	w.kv(familyKey, 8, "qwen3_asr")

	info, err := Open(w.file(t))
	if err != nil {
		t.Fatal(err)
	}
	if info.Family != "qwen3_asr" {
		t.Errorf("Family = %q, want qwen3_asr", info.Family)
	}
}

func TestOpenSaysWhichKindOfWrongFile(t *testing.T) {
	// Not a GGUF at all.
	notGGUF := filepath.Join(t.TempDir(), "readme.txt")
	if err := os.WriteFile(notGGUF, []byte("checkpoint notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(notGGUF); err == nil {
		t.Fatal("a text file was accepted")
	} else if !strings.Contains(err.Error(), "not a GGUF") {
		t.Errorf("error = %q, want it to say the file is not a GGUF", err)
	}

	// A GGUF, but not one audio.cpp can serve: the name the user gave was a different model.
	w := &writer{}
	w.kv("general.architecture", 8, "whisper")
	_, err := Open(w.file(t))
	if err == nil {
		t.Fatal("a GGUF with no model spec was accepted")
	}
	if !strings.Contains(err.Error(), "whisper") || !strings.Contains(err.Error(), "model spec") {
		t.Errorf("error = %q, want it to name the architecture and say there is no model spec", err)
	}

	if _, err := Open(filepath.Join(t.TempDir(), "absent.gguf")); err == nil {
		t.Error("a missing file was accepted")
	}
}
