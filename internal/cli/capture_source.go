package cli

import "strings"

// defaultSource is the capture source used when capture_source is empty.
func defaultSource() string {
	output, err := probe("pactl", "get-default-source")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}
