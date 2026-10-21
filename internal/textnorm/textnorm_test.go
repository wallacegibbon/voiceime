package textnorm

import "testing"

func TestNormalizeTraditionalToSimplified(t *testing.T) {
	normalizer := New("zh", Options{})
	cases := map[string]string{
		"我們今天討論這個軟體的進度。": "我们今天讨论这个软体的进度。",
		"臺灣的臺北是中華民國的首都。": "台湾的台北是中华民国的首都。",
		"他說裏面還有一些資料。":    "他说里面还有一些资料。",
		"乾隆皇帝":           "乾隆皇帝",
		"頭髮":             "头发",
		"幹部":             "干部",
		"麵條":             "面条",
	}
	for input, want := range cases {
		if got := normalizer.Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

// Script conversion is a single pass over OpenCC's t2s data, and that data is
// not idempotent: it preserves some forms that it would convert if they were
// the input. This is why the transcript stores raw chunks and normalizes on
// read instead of normalizing before Place.
func TestScriptConversionIsSinglePass(t *testing.T) {
	normalizer := New("zh", Options{})
	if got := normalizer.Normalize("乾圖"); got != "乾图" {
		t.Errorf("Normalize(乾圖) = %q, want the first pass 乾图", got)
	}
	if got := normalizer.Normalize("乾图"); got != "干图" {
		t.Errorf("Normalize(乾图) = %q, want OpenCC's second pass 干图 (the hazard this design avoids)", got)
	}
	if got := normalizer.Normalize("乾隆皇帝"); got != "乾隆皇帝" {
		t.Errorf("Normalize(乾隆皇帝) = %q, want the preserved name", got)
	}
}

func TestPreserveOption(t *testing.T) {
	input := "臺灣，二零二五年。"
	if got := New("zh", Options{Script: ScriptPreserve}).Normalize(input); got != input {
		t.Errorf("preserve: %q, want %q unchanged", got, input)
	}
	if got := New("zh", Options{}).Normalize(input); got != "台湾，二零二五年。" {
		t.Errorf("default: %q, want the script converted and the numerals kept", got)
	}
}

func TestNonChineseIsUntouched(t *testing.T) {
	input := "一二三 臺灣"
	for _, language := range []string{"en", "ja", "ko", ""} {
		if got := New(language, Options{}).Normalize(input); got != input {
			t.Errorf("New(%q) changed %q to %q", language, input, got)
		}
	}
}

func TestChineseLanguageVariantsAreNormalized(t *testing.T) {
	for _, language := range []string{"zh", "zh-CN", "zh-Hant", "zh_TW", "cmn", "yue", "Chinese"} {
		if got := New(language, Options{}).Normalize("臺灣"); got != "台湾" {
			t.Errorf("New(%q).Normalize = %q, want 台湾", language, got)
		}
	}
}
