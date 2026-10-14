package config

import "os"

// WriteFileForTest writes raw bytes for the deliberately malformed config test.
func WriteFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// UserHomeDirForTest exposes the home lookup so a test can skip when there is
// none, instead of asserting on a machine-dependent value.
func UserHomeDirForTest() (string, error) { return os.UserHomeDir() }
