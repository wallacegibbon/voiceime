package textnorm

import "testing"

func TestFinalDropsTheTrailingPeriod(t *testing.T) {
	cases := map[string]string{
		"嗯。":           "嗯",
		"你好。":          "你好",
		"Hello world.": "Hello world",
		"你好. ":         "你好",
		"你好。。":         "你好",
		"。。":           "",
		"":             "",
		// Marks that carry meaning the speaker actually said stay.
		"你好？":  "你好？",
		"太好了！": "太好了！",
		"第一，":  "第一，",
		// A period that is not at the end is not touched.
		"3.14":  "3.14",
		"你好。再见": "你好。再见",
	}
	for input, want := range cases {
		if got := Final(input); got != want {
			t.Errorf("Final(%q) = %q, want %q", input, got, want)
		}
	}
}
