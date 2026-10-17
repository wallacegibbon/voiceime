package cli

import "strings"

// defaultSource is the capture source used when capture_source is empty.
//
// It is the only part of the old `devices` command that survived: `doctor` prints it
// beside the capture check, which is where a user who needs to pick a microphone looks
// anyway - and where the listening state is actually verified, rather than listed.
func defaultSource() string {
	output, err := probe("pactl", "get-default-source")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}
