package textnorm

import (
	"strings"
	"testing"
)

func TestNormalizeChineseNumerals(t *testing.T) {
	normalizer := New("zh", Options{})
	cases := map[string]string{
		// What Qwen3-ASR actually returned for the reference recording.
		"今天是二零二五年十月二十一号。我有一千二百三十四个苹果，价格是五千六百七十八块钱。": "今天是2025年10月21号。我有1234个苹果，价格是5678块钱。",
		// Dates, times, decimals, percentages and fractions.
		"二零二五年十月二十一号": "2025年10月21号",
		"下午三点半":       "下午3点半",
		"三点一四":        "3.14",
		"零点五":         "0.5",
		"百分之三十":       "30%",
		"百分之三十点五":     "30.5%",
		"千分之五":        "5‰",
		"三分之一":        "1/3",
		"十分之三":        "3/10",
		"负三十":         "-30",
		"负三分之一":       "-1/3",
		"负十分之三":       "-3/10",
		// Measure words and colloquial numbers.
		"一个人":  "1个人",
		"两个人":  "2个人",
		"两三天":  "2-3天",
		"七八个":  "7-8个",
		"十分钟":  "10分钟",
		"一千二":  "1200",
		"三千五":  "3500",
		"一万二":  "12000",
		"十块钱":  "10块钱",
		"十二个人": "12个人",
		"二十几个": "20几个",
		// Digit strings: years, phones, codes.
		"一三八零零一三八零零零": "13800138000",
		"三八零零":        "3800",
		"幺三八":         "138",
		"幺六":          "16",
		"BF幺六":        "BF16",
		// Place-value forms and their colloquial tails.
		"一百二十三":      "123",
		"一百零二":       "102",
		"一千零二十":      "1020",
		"一万零二百":      "10200",
		"三万五千":       "35000",
		"一亿三千万":      "130000000",
		"十三":         "13",
		"二十":         "20",
		"三十万":        "300000",
		"两千五":        "2500",
		"壹仟贰佰叁拾肆":    "1234",
		"壹点贰":        "1.2",
		"十二万三千四百五十六": "123456",
		"一万亿":        "1000000000000",
		// More date and measure contexts.
		"两点半":    "2点半",
		"十二月":    "12月",
		"十一月":    "11月",
		"一号":     "1号",
		"第一":     "第1",
		"第一次":    "第1次",
		"一个半":    "1个半",
		"二十几个人":  "20几个人",
		"百分之一":   "1%",
		"千分之一":   "1‰",
		"万分之一":   "1‱",
		"负零点五":   "-0.5",
		"百分之零点五": "0.5%",
		// A decimal is numeric because of its form, not because a measure
		// word follows it: 一点五左右 is still 1.5.
		"一点五左右": "1.5左右",
		"零点五左右": "0.5左右",
		"负百分之五": "-5%",
		"百分之负五": "-5%",
		// 一点 is also a time; only readings that can only be a time convert.
		"一点钟": "1点钟",
		"一点半": "1点半",
		"一点整": "1点整",
		// A trailing point is a decimal the next chunk has not completed yet.
		"百分之三点": "百分之三点",
		// 两 after a unit is the measure word (tael), not the digit two.
		"两千两百": "2200",
		"十两银子": "十两银子",
		// Already Arabic: idempotent.
		"2025年10月21号，我有1234个苹果。": "2025年10月21号，我有1234个苹果。",
		"这个项目投资了1万亿":             "这个项目投资了1万亿",
	}
	for input, want := range cases {
		if got := normalizer.Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeLeavesWordsAlone(t *testing.T) {
	normalizer := New("zh", Options{})
	cases := []string{
		"我们一起去公园散步吧。",
		"这是一样的。",
		"他一直很好。",
		"我们一定要成功。",
		"一般情况下没有问题。",
		"千万要记住。",
		"十分感谢。",
		"万里长城。",
		"老百姓的生活。",
		"万岁。",
		"千方百计。",
		"七零八落。",
		"三三两两。",
		"一五一十地说。",
		"他是唯一的人选。",
		"大家统一意见。",
		"星期一开会。",
		"初一十五。",
		"乱七八糟。",
		"三心二意。",
		"五颜六色。",
		"十全十美。",
		"二氧化碳。",
		// Numerals that look like quantities but are part of ordinary words.
		// These pin the conservative rule: a miss is acceptable, a rewrite is
		// not. 分 is not a measure word by itself, which is what keeps
		// 百分比/百分制/千分尺 out of the converter.
		"百分比。",
		"百分制。",
		"千分尺。",
		"五花八门。",
		"百步穿杨。",
		"七步成诗。",
		"三人成虎。",
		"四分五裂。",
		"六根清净。",
		"千篇一律。",
		"千头万绪。",
		"百家争鸣。",
		"一分为二。",
		"一日千里。",
		"一人得道。",
		"一家之言。",
		"一门心思。",
		"一本万利。",
		"一声不吭。",
		"一笔勾销。",
		"一步登天。",
		"一块儿去。",
		"一年半载。",
		"一天到晚。",
		"一分一秒。",
		"一分一毫。",
		"三五成群。",
		"三五好友。",
		"二八年华。",
		"三七二十一。",
		"三八妇女节。",
		"五一。",
		"六一儿童节。",
		"八一建军节。",
		"五四运动。",
		"九一八事变。",
		"十一国庆节。",
		"十年寒窗。",
		"一步之遥。",
		"十万八千里。",
		"三点水。",
		"两点水。",
		"五十步笑百步。",
		"一点。",
		"有一点累。",
		"再更好看一点。",
		// 两 is also the measure word for a tael, and a two-character run
		// ending in it is not a digit string.
		"一两银子。",
		"三两银子。",
		"十两银子。",
		"十二两银子。",
		"三两个。",
		// 升 is a verb in 升级, so a lone digit before it must not convert.
		"二升级。",
		"从V二升级到V三。",
		// A word ending in a numeral followed by a quantity keeps the quantity:
		// consuming only the word would give 唯11家.
		"唯一一家。",
		"唯一一个。",
		// Malformed place-value runs and values that would overflow int64 are
		// left as written, never turned into a negative or wrapped number.
		"一百二千。",
		"九零千亿三亿零六。",
		"一二三，走。", // digit sequence, converted below; this is not a word case
	}
	for _, input := range cases {
		got := normalizer.Normalize(input)
		if input == "一二三，走。" {
			if got != "123，走。" {
				t.Errorf("Normalize(%q) = %q, want digits", input, got)
			}
			continue
		}
		if got != input {
			t.Errorf("Normalize(%q) = %q, want unchanged", input, got)
		}
	}
}

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

// Number normalization is idempotent: Arabic digits and punctuation are not
// touched, and a preserved exception matches again on the second pass.
func TestNumberNormalizationIsIdempotent(t *testing.T) {
	normalizer := New("zh", Options{Script: ScriptPreserve})
	input := "二零二五年十月二十一号，一千二百三十四個蘋果，百分之三十點五，十分感謝。"
	once := normalizer.Normalize(input)
	twice := normalizer.Normalize(once)
	if once != twice {
		t.Errorf("not idempotent: once = %q, twice = %q", once, twice)
	}
}

// Script conversion is a single pass over OpenCC's t2s data, and that data is
// not idempotent: it preserves some forms that it would convert if they were
// the input. This is why the transcript stores raw chunks and normalizes on
// read instead of normalizing before Place.
func TestScriptConversionIsSinglePass(t *testing.T) {
	normalizer := New("zh", Options{Numbers: NumbersPreserve})
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

func TestPreserveOptions(t *testing.T) {
	input := "臺灣，二零二五年。"
	if got := New("zh", Options{Script: ScriptPreserve, Numbers: NumbersPreserve}).Normalize(input); got != input {
		t.Errorf("both preserved: %q", got)
	}
	if got := New("zh", Options{Script: ScriptPreserve}).Normalize(input); got != "臺灣，2025年。" {
		t.Errorf("script preserved: %q", got)
	}
	if got := New("zh", Options{Numbers: NumbersPreserve}).Normalize(input); got != "台湾，二零二五年。" {
		t.Errorf("numbers preserved: %q", got)
	}
	// The number rules must understand Traditional forms even when the user
	// asked to keep Traditional characters: the script conversion is off, so
	// 千萬 and 萬分之 reach the number pass as written.
	if got := New("zh", Options{Script: ScriptPreserve}).Normalize("千萬要記住。"); got != "千萬要記住。" {
		t.Errorf("traditional adverb: %q", got)
	}
	if got := New("zh", Options{Script: ScriptPreserve}).Normalize("萬分之三"); got != "3‱" {
		t.Errorf("traditional per-ten-thousand: %q", got)
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
		if got := New(language, Options{}).Normalize("二零二五"); got != "2025" {
			t.Errorf("New(%q).Normalize = %q, want 2025", language, got)
		}
	}
}

// The fast path returns before the parser when the text contains no numeral, so
// every rune the parser understands must be in numeralRunes. Adding a numeral to
// one of the tables without adding it here would make that numeral silently stop
// being normalized.
func TestNumeralRunesCoversTheTables(t *testing.T) {
	for r := range chineseDigit {
		if !strings.ContainsRune(numeralRunes, r) {
			t.Errorf("chineseDigit has %q, which numeralRunes omits", r)
		}
	}
	for r := range chineseUnit {
		if !strings.ContainsRune(numeralRunes, r) {
			t.Errorf("chineseUnit has %q, which numeralRunes omits", r)
		}
	}
	for _, r := range "点點负負" {
		if !strings.ContainsRune(numeralRunes, r) {
			t.Errorf("numeralRunes omits %q", r)
		}
	}
}
