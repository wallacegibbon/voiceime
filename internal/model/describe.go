package model

import "strconv"

// Sizes renders a checkpoint's size the way it is quoted back to a person: big files in GiB,
// the rest in MiB. A 62 MiB model and a 1.07 GiB model are different decisions, and "1151272416
// bytes" hides that.
func Sizes(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 2, 64) + " GiB"
	case bytes >= 1<<20:
		return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64) + " MiB"
	default:
		return strconv.FormatInt(bytes, 10) + " bytes"
	}
}
