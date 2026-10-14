package audio

import "os"

// WriteFileForTest writes raw bytes, for the tests that feed ReadWAV something
// that is deliberately not a WAV file.
func WriteFileForTest(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
