// Package model reads the checkpoint a user has named. There is no collection of models to
// manage: a model is one GGUF file, its path is the setting, and what it is comes from the
// file itself.
package model

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// The audio.cpp server entry needs one fact about a checkpoint that its path does not carry:
// the family, which chooses the loader - app/server/config.cpp in audio.cpp requires it.
// audio.cpp writes its model spec into every GGUF it produces, so the answer is in the file:
//
//	audiocpp.model_spec.family = "sense_asr"
//
// Reading the GGUF's key-value section is enough. It is a few hundred bytes at the start of
// the file: nothing is loaded, decoded or indexed, no directory is scanned, and no other
// program has to be asked.
const (
	magic           = "GGUF"
	familyKey       = "audiocpp.model_spec.family"
	architectureKey = "general.architecture"
	specKey         = "audiocpp.model_spec.json"
	nameKey         = "general.name"
)

// Says reports whether the file names a task: an empty Tasks is "the file did not say", and
// an answer built on it would be a guess.
func (i Info) Says() bool { return len(i.Tasks) > 0 }

// ASR reports whether the file says it transcribes speech.
func (i Info) ASR() bool {
	for _, task := range i.Tasks {
		if task == "asr" {
			return true
		}
	}
	return false
}

// Info is what a checkpoint file says about itself.
type Info struct {
	Path   string `json:"path"`
	Family string `json:"family"`
	// Architecture is what the file turns out to be when it is not a speech model, which
	// is what makes the difference between "wrong file" and "file audio.cpp cannot load".
	Architecture string `json:"architecture"`
	Name         string `json:"name,omitempty"`
	// Tasks is what the file's own spec says it does ("asr", "tts", ...). Some
	// checkpoints embed a full spec and some embed only a family, so an empty Tasks
	// means the file did not say - not that it does nothing.
	Tasks     []string `json:"tasks,omitempty"`
	SizeBytes int64    `json:"size_bytes"`
}

// Open reads a checkpoint's metadata. The errors are written for someone holding a file they
// were told to point voiceime at, so each says which of the two problems it is: the file is
// not a GGUF, or it is a GGUF that audio.cpp has no spec for.
func Open(path string) (Info, error) {
	file, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer file.Close()

	info := Info{Path: path}
	if stat, err := file.Stat(); err == nil {
		info.SizeBytes = stat.Size()
	}

	reader := bufio.NewReader(file)
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return Info{}, fmt.Errorf("%s: %v", path, err)
	}
	if string(header[:]) != magic {
		return Info{}, fmt.Errorf("%s is not a GGUF file (it starts with %q, not %q)", path, string(header[:]), magic)
	}
	if _, err := reader.Discard(4); err != nil { // the format version: audio.cpp's writer is the only producer
		return Info{}, fmt.Errorf("%s: truncated header: %v", path, err)
	}
	if _, err := readU64(reader); err != nil { // tensor count: the tensors themselves are not read
		return Info{}, fmt.Errorf("%s: truncated header: %v", path, err)
	}
	keywordCount, err := readU64(reader)
	if err != nil {
		return Info{}, fmt.Errorf("%s: truncated header: %v", path, err)
	}

	for i := uint64(0); i < keywordCount; i++ {
		key, err := readString(reader)
		if err != nil {
			return Info{}, fmt.Errorf("%s: metadata key %d is unreadable: %v", path, i, err)
		}
		value, err := readValue(reader, key == familyKey || key == architectureKey || key == nameKey || key == specKey)
		if err != nil {
			return Info{}, fmt.Errorf("%s: the metadata after %q is unreadable: %v", path, key, err)
		}
		switch key {
		case familyKey:
			info.Family = value
		case architectureKey:
			info.Architecture = value
		case nameKey:
			info.Name = value
		case specKey:
			// The spec is JSON, and only the part that says what the model does is read:
			// enough to say "that one writes text out, it does not transcribe" when the
			// file bothers to say so.
			var spec struct {
				Tasks []string `json:"tasks"`
			}
			if err := json.Unmarshal([]byte(value), &spec); err == nil {
				info.Tasks = spec.Tasks
			}
		}
	}

	if info.Family == "" {
		architecture := strings.TrimSpace(info.Architecture)
		if architecture == "" {
			architecture = "no architecture recorded"
		}
		return Info{}, fmt.Errorf(
			"%s is a GGUF (%s) with no audio.cpp model spec in it, so there is no family to serve it under: convert it with audio.cpp, or point model at a checkpoint audio.cpp produced",
			path, architecture)
	}
	return info, nil
}

// readValue reads a metadata value, starting at its type. Types are uint32 in GGUF: a byte
// here misaligns everything that follows, which is what the real-file test caught.
func readValue(reader *bufio.Reader, wanted bool) (string, error) {
	kind, err := readU32(reader)
	if err != nil {
		return "", err
	}
	return readValueOfKind(reader, kind, wanted)
}

// readValueOfKind reads a value whose type is already known. It matters for arrays: their
// elements are encoded according to the element type the array declares, so each element does
// not carry a type byte of its own. A vocabulary is an array of hundreds of thousands of
// strings, so skipping one must not build them.
func readValueOfKind(reader *bufio.Reader, kind uint32, wanted bool) (string, error) {
	switch kind {
	case 8: // string
		value, err := readString(reader)
		if err != nil {
			return "", err
		}
		if wanted {
			return value, nil
		}
		return "", nil
	case 9, 13: // array; 13 is the GGUF v3 namespace, encoded as an array of strings
		elementKind, err := readU32(reader)
		if err != nil {
			return "", err
		}
		count, err := readU64(reader)
		if err != nil {
			return "", err
		}
		var first string
		for i := uint64(0); i < count; i++ {
			value, err := readValueOfKind(reader, elementKind, wanted && first == "")
			if err != nil {
				return "", err
			}
			if value != "" {
				first = value
			}
		}
		return first, nil
	default:
		size, ok := scalarSize(kind)
		if !ok {
			return "", fmt.Errorf("metadata value of unknown type %d", kind)
		}
		if _, err := reader.Discard(size); err != nil {
			return "", err
		}
		return "", nil
	}
}

// scalarSize is the width of each fixed-size GGUF metadata type.
func scalarSize(kind uint32) (int, bool) {
	switch kind {
	case 0, 1, 7: // u8, i8, bool
		return 1, true
	case 2, 3: // u16, i16
		return 2, true
	case 4, 5, 6: // u32, i32, f32
		return 4, true
	case 10, 11, 12: // u64, i64, f64
		return 8, true
	}
	return 0, false
}

func readU32(reader *bufio.Reader) (uint32, error) {
	var raw [4]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(raw[:]), nil
}

func readU64(reader *bufio.Reader) (uint64, error) {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(raw[:]), nil
}

func readString(reader *bufio.Reader) (string, error) {
	length, err := readU64(reader)
	if err != nil {
		return "", err
	}
	if length > 1<<24 { // no metadata string is tens of megabytes; the stream is lost
		return "", fmt.Errorf("implausible string length %s", strconv.FormatUint(length, 10))
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	return string(raw), nil
}
