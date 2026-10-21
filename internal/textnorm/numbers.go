package textnorm

// Chinese numeral normalization.
//
// Qwen3-ASR, the repair model the reference configuration uses, has no inverse
// text normalization: it writes what it hears as words, so "2025年10月21号" comes
// back as "二零二五年十月二十一号" and "1234个" as "一千二百三十四个". The
// streaming model (SenseVoice) does have ITN, but the repair replaces its text, so
// the committed sentence is the one without digits.
//
// The conversion here is deliberately conservative. A blanket "every numeral
// character becomes a digit" rule destroys common words - 一起, 十分, 千万, 万岁,
// 千方百计 - so a run is only converted when it looks like a number (it has a
// place-value unit, it is a digit string, or it is a single digit in a clear
// numeric context), and a lexicon of idioms and fixed expressions is left alone
// first.
//
// Examples:
//
//	二零二五年十月二十一号   -> 2025年10月21号
//	一千二百三十四个         -> 1234个
//	百分之三十点五           -> 30.5%
//	三分之一                 -> 1/3
//	十分好                   -> 十分好          (the adverb, not ten minutes)
//	千万要记住               -> 千万要记住      (the adverb, not ten million)
//	一个人                   -> 1个人
//	一起去                   -> 一起去
//
// The number conversion is idempotent: Arabic digits and punctuation are not
// touched, so applying it again cannot change its own output.

import (
	"math"
	"strconv"
	"strings"
	"unicode"
)

// chineseDigit maps a numeral character to its value. 两/兩 is 2 in counting
// contexts ("两个"), while 幺 is how 1 is read in phone numbers and codes.
var chineseDigit = map[rune]int{
	'零': 0, '〇': 0, '○': 0,
	'一': 1, '幺': 1, '壹': 1,
	'二': 2, '两': 2, '兩': 2, '贰': 2, '貳': 2,
	'三': 3, '叁': 3, '參': 3,
	'四': 4, '肆': 4,
	'五': 5, '伍': 5,
	'六': 6, '陆': 6, '陸': 6,
	'七': 7, '柒': 7,
	'八': 8, '捌': 8,
	'九': 9, '玖': 9,
}

// chineseUnit maps a place-value character to its value. The financial forms are
// included because dictation of money sometimes produces them.
var chineseUnit = map[rune]int{
	'十': 10, '拾': 10,
	'百': 100, '佰': 100,
	'千': 1000, '仟': 1000,
	'万': 10000, '萬': 10000,
	'亿': 100000000, '億': 100000000,
}

const (
	tenThousand    = 10000
	hundredMillion = 100000000
)

func isNumeral(r rune) bool {
	_, digit := chineseDigit[r]
	if digit {
		return true
	}
	_, unit := chineseUnit[r]
	return unit || r == '点' || r == '點'
}

func isDigitRune(r rune) bool {
	_, ok := chineseDigit[r]
	return ok
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func isUnitRune(r rune) bool {
	_, ok := chineseUnit[r]
	return ok
}

// numeralRunes is every rune the number normalizer can act on. It lets ordinary
// text leave immediately: the transcript normalizes on every preedit update, and
// most of a Chinese sentence contains no numeral at all.
const numeralRunes = "零〇○一二两兩三四五六七八九十百千万亿點点负負幺壹贰貳叁參肆伍陆陸柒捌玖拾佰仟萬億"

// normalizeNumbers rewrites Chinese numerals as Arabic digits.
func normalizeNumbers(text string) string {
	if text == "" || !strings.ContainsAny(text, numeralRunes) {
		return text
	}
	runes := []rune(text)
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(runes); {
		// Everything the normalizer understands starts with a numeral or a
		// minus sign, except the fixed expressions that begin with another
		// character (乱七八糟, 老百姓, 统一). Check those here, then skip
		// ordinary prose past the prefix and measure-word checks below.
		if !isNumeral(runes[index]) && !isNegative(runes[index]) {
			if length := exceptionAt(runes, index); length > 0 {
				out.WriteString(string(runes[index : index+length]))
				index += length
				continue
			}
			out.WriteRune(runes[index])
			index++
			continue
		}
		// Percent and per-mille are the two "X分之" forms that are not
		// fractions: 百分之三十 is 30%, not 30/100. A prefix with no number
		// after it is written literally, because a preedit can end mid-phrase.
		if replacement, next := convertPercentAt(runes, index); next > index {
			out.WriteString(replacement)
			index = next
			continue
		}

		// A negative number or fraction: 负三十 is -30, 负三分之一 is -1/3.
		if isNegative(runes[index]) &&
			index+1 < len(runes) && isNumeral(runes[index+1]) {
			if replacement, next, ok := convertFraction(runes, index+1); ok {
				out.WriteString("-" + replacement)
				index = next
				continue
			}
			end := numeralRunEnd(runes, index+1)
			if end > index+1 {
				if replacement, ok := convertRun(runes, index+1, end, true); ok {
					out.WriteString("-" + replacement)
					index = end
					continue
				}
			}
		}

		// A fraction: "三分之一" is read denominator first, so it is 1/3.
		if isNumeral(runes[index]) {
			if replacement, next, ok := convertFraction(runes, index); ok {
				out.WriteString(replacement)
				index = next
				continue
			}
		}

		if length := exceptionAt(runes, index); length > 0 {
			out.WriteString(string(runes[index : index+length]))
			index += length
			continue
		}

		if !isNumeral(runes[index]) {
			out.WriteRune(runes[index])
			index++
			continue
		}

		end := numeralRunEnd(runes, index)
		if end <= index {
			out.WriteRune(runes[index])
			index++
			continue
		}
		replacement, ok := convertRun(runes, index, end, false)
		if !ok {
			out.WriteString(string(runes[index:end]))
			index = end
			continue
		}
		out.WriteString(replacement)
		index = end
	}
	return out.String()
}

// numeralRunEnd returns the end of the numeral run starting at start. A 点 is
// part of the run only when a digit follows it: in "一点钟" the point is a measure
// word (o'clock), while in "一点五" it is a decimal point.
func numeralRunEnd(runes []rune, start int) int {
	end := start
	for end < len(runes) && isNumeral(runes[end]) {
		if runes[end] == '点' || runes[end] == '點' {
			if end+1 >= len(runes) || !isDigitRune(runes[end+1]) {
				break
			}
			// A point followed by a digit is a decimal point only when it is
			// not telling the time. 两点零五分 is 2点05分 (and 十点一刻 is
			// 10点一刻), while 三点五 is 3.5 and 三点五米 is 3.5米. The time
			// reading is the one whose digit run ends in a clock word.
			if clockQuantityFollows(runes, end+1) {
				break
			}
		}
		end++
	}
	return end
}

// clockQuantityFollows reports whether the numeral run starting at index ends
// in a minute or quarter-hour word: 分, 钟, 刻 or 半. It skips units as well as
// digits, so 三点五十分 is a time (3点50分) while 三点五十 is left alone.
func clockQuantityFollows(runes []rune, index int) bool {
	for index < len(runes) && (isDigitRune(runes[index]) || isUnitRune(runes[index])) {
		index++
	}
	for _, word := range []string{"分", "钟", "鐘", "刻", "半"} {
		if hasPrefix(runes, index, word) {
			return true
		}
	}
	return false
}

// convertRun turns one numeral run into digits, or reports that it is not a
// number. The context rules live here so every caller gets the same answer.
func convertRun(runes []rune, start, end int, force bool) (string, bool) {
	run := runes[start:end]
	if len(run) == 0 {
		return "", false
	}
	// A Chinese numeral glued to an Arabic digit is already a mixed form the
	// model wrote ("1万"). Converting the Chinese half would turn it into
	// "110000"; leaving it keeps what the user can already read.
	if (start > 0 && isASCIIDigit(runes[start-1])) || (end < len(runes) && isASCIIDigit(runes[end])) {
		return "", false
	}
	// 初一 is the first day of a lunar month, not "初1"; 星期一 is a weekday.
	if start > 0 && (runes[start-1] == '初' || runes[start-1] == '周' || runes[start-1] == '週' ||
		runes[start-1] == '期' || runes[start-1] == '拜') {
		return "", false
	}
	// 两/兩 is both the digit 2 and the measure word "tael". After the first
	// position it is only a digit when a place-value unit or a measure word
	// follows it; "一两银子" is one tael, not 12, and "十两银子" is ten taels.
	for index := start + 1; index < end; index++ {
		if runes[index] != '两' && runes[index] != '兩' {
			continue
		}
		if index+1 < end && isUnitRune(runes[index+1]) {
			continue
		}
		if followedByMeasureWord(runes, index+1) {
			continue
		}
		return "", false
	}

	// A decimal number, e.g. 三点一四 -> 3.14.
	if point := indexOfPoint(run); point >= 0 {
		integer, ok := convertInteger(run[:point], runes, start, end, true, force)
		if !ok {
			return "", false
		}
		decimal := run[point+1:]
		if len(decimal) == 0 {
			return "", false
		}
		var fraction strings.Builder
		for _, r := range decimal {
			digit, ok := chineseDigit[r]
			if !ok {
				return "", false
			}
			fraction.WriteString(strconv.Itoa(digit))
		}
		return integer + "." + fraction.String(), true
	}

	return convertInteger(run, runes, start, end, false, force)
}

func indexOfPoint(run []rune) int {
	for index, r := range run {
		if r == '点' || r == '點' {
			return index
		}
	}
	return -1
}

// convertInteger handles a run with no decimal point. allDigits selects the
// digit-by-digit reading (二零二五 -> 2025) rather than a place-value parse.
func convertInteger(run []rune, runes []rune, start, end int, decimal, force bool) (string, bool) {
	if len(run) == 0 {
		return "", false
	}
	allDigits := true
	for _, r := range run {
		if !isDigitRune(r) {
			allDigits = false
			break
		}
	}
	if allDigits {
		if len(run) == 1 && !force && !decimal {
			if !singleDigitIsNumeric(runes, start, end) {
				return "", false
			}
		}
		if len(run) == 2 && !decimal {
			if first, second, ok := numeralRange(run, runes, end); ok {
				return strconv.Itoa(first) + "-" + strconv.Itoa(second), true
			}
			// 两 after the first position is a range or a measure word, not a
			// digit: 三两个 is not 32, and 一两银子 is not 12. numeralRange
			// handled the ascending range; anything else is left alone.
			if run[1] == '两' || run[1] == '兩' {
				return "", false
			}
		}
		var out strings.Builder
		for _, r := range run {
			out.WriteString(strconv.Itoa(chineseDigit[r]))
		}
		return out.String(), true
	}

	value, ok := parseChineseInteger(run)
	if !ok {
		return "", false
	}
	// A bare 十/百/千/万/亿 is only a number when something follows that makes it
	// one. "百姓" and "万岁" stay words; "十个" and "一百个" become digits.
	if !force && len(run) == 1 && isUnitRune(run[0]) && !decimal {
		if !singleUnitIsNumeric(runes, start, end) {
			return "", false
		}
	}
	// 千万/千萬 on its own is almost always the adverb ("be sure to"), while
	// 一千万 is the number.
	if !force && len(run) == 2 && (string(run) == "千万" || string(run) == "千萬") && !decimal {
		if !followedByMeasureWord(runes, end) {
			return "", false
		}
	}
	return strconv.FormatInt(value, 10), true
}

// singleDigitIsNumeric decides whether a lone 一/二/... is a number. It is one
// when a measure word follows ("一个人", "两天"), or when it stands alone as an
// answer ("一。"), but not when it is glued into a word ("统一", "万一").
func singleDigitIsNumeric(runes []rune, start, end int) bool {
	if start > 0 && runes[start-1] == '第' {
		return true
	}
	// 一点 is the adverb "a little" (有一点累, 再好看一点), not one o'clock.
	// Only the readings that can only be a time are numeric: 一点钟, 一点半,
	// 一点整, or a decimal (handled before this function as 一点五).
	if end < len(runes) && (runes[end] == '点' || runes[end] == '點') &&
		chineseDigit[runes[start]] == 1 {
		after := end + 1
		if after >= len(runes) {
			return false
		}
		if isDigitRune(runes[after]) {
			return true
		}
		for _, follower := range []string{"钟", "鐘", "整", "半"} {
			if hasPrefix(runes, after, follower) {
				return true
			}
		}
		return false
	}
	if followedByMeasureWord(runes, end) {
		return true
	}
	if start == 0 && atBoundary(runes, end) {
		return true
	}
	if start > 0 && atBoundary(runes, end) && isSentenceBoundary(runes[start-1]) {
		return true
	}
	return false
}

// singleUnitIsNumeric is the same rule for 十/百/千/万/亿: they need a measure
// word or an ordinal prefix, because standing alone they are usually part of a
// word (百姓, 万岁, 千秋).
func singleUnitIsNumeric(runes []rune, start, end int) bool {
	if start > 0 && runes[start-1] == '第' {
		return true
	}
	// 十分 is the adverb "very" unless a unit of time or a fraction follows:
	// 十分钟, 十分之三 and 十点半 are numbers, 十分感谢 is not.
	if end-start == 1 && chineseUnit[runes[start]] == 10 && hasPrefix(runes, end, "分") {
		after := end + 1
		for _, follower := range []string{"钟", "鐘", "之", "秒", "点", "點", "半"} {
			if hasPrefix(runes, after, follower) {
				return true
			}
		}
		if after < len(runes) && isDigitRune(runes[after]) {
			return true
		}
		return atBoundary(runes, after)
	}
	if followedByMeasureWord(runes, end) {
		return true
	}
	return false
}

// numeralRange reads 两三 / 七八 as an approximate range when a measure word
// follows: 两三天 is 2-3天, not 23天.
func numeralRange(run []rune, runes []rune, end int) (int, int, bool) {
	if len(run) != 2 {
		return 0, 0, false
	}
	first, ok := chineseDigit[run[0]]
	if !ok || first == 0 {
		return 0, 0, false
	}
	second, ok := chineseDigit[run[1]]
	if !ok || second == 0 {
		return 0, 0, false
	}
	if second <= first {
		return 0, 0, false
	}
	// A range needs a measure word: 两三天 is a range, while 幺六 and 七八 are
	// digit strings unless something like 个/年 follows. Without the measure
	// word, BF幺六 is BF16, not BF1-6.
	if !followedByMeasureWord(runes, end) {
		return 0, 0, false
	}
	return first, second, true
}

// percentPrefixes are the three "X分之" forms that are percentages rather than
// fractions, in the order the matcher must try them.
var percentPrefixes = []struct {
	word   string
	suffix string
}{
	{"百分之", "%"},
	{"千分之", "‰"},
	{"万分之", "‱"},
	{"萬分之", "‱"},
}

// convertPercentAt converts a percentage at index and returns the text to emit
// and where to continue. When the prefix is present but no number follows it,
// next advances past the prefix and the literal prefix is returned: a preedit can
// end in the middle of "百分之", and turning that into "100分之" would be wrong.
func convertPercentAt(runes []rune, index int) (string, int) {
	start := index
	negative := false
	if isNegative(runes[index]) {
		if _, _, found := percentPrefixAt(runes, index+1); found {
			negative = true
			index++
		} else {
			return "", start
		}
	}
	suffix, after, found := percentPrefixAt(runes, index)
	if !found {
		return "", start
	}
	literalEnd := after
	if after < len(runes) && isNegative(runes[after]) {
		negative = true
		after++
		literalEnd = after
	}
	end := numeralRunEnd(runes, after)
	if end <= after {
		return string(runes[start:literalEnd]), literalEnd
	}
	// A trailing point is a decimal that has not arrived yet ("百分之三点").
	// Converting the integer now would show "3%点"; leave the phrase as the
	// model wrote it until the next chunk completes the number.
	if end < len(runes) && (runes[end] == '点' || runes[end] == '點') &&
		(end+1 >= len(runes) || atBoundary(runes, end+1)) {
		return string(runes[start : end+1]), end + 1
	}
	replacement, ok := convertRun(runes, after, end, true)
	if !ok {
		return string(runes[start:literalEnd]), literalEnd
	}
	if negative {
		replacement = "-" + replacement
	}
	return replacement + suffix, end
}

func percentPrefixAt(runes []rune, index int) (suffix string, after int, found bool) {
	for _, prefix := range percentPrefixes {
		if hasPrefix(runes, index, prefix.word) {
			return prefix.suffix, index + len([]rune(prefix.word)), true
		}
	}
	return "", index, false
}

func isNegative(r rune) bool { return r == '负' || r == '負' }

// convertFraction converts "X分之Y" to Y/X: 三分之一 is 1/3.
func convertFraction(runes []rune, start int) (string, int, bool) {
	numeratorEnd := numeralRunEnd(runes, start)
	if numeratorEnd <= start || !hasPrefix(runes, numeratorEnd, "分之") {
		return "", start, false
	}
	denominatorStart := numeratorEnd + len([]rune("分之"))
	denominatorEnd := numeralRunEnd(runes, denominatorStart)
	if denominatorEnd <= denominatorStart {
		return "", start, false
	}
	numerator, ok := convertRun(runes, start, numeratorEnd, true)
	if !ok {
		return "", start, false
	}
	denominator, ok := convertRun(runes, denominatorStart, denominatorEnd, true)
	if !ok {
		return "", start, false
	}
	return denominator + "/" + numerator, denominatorEnd, true
}

// parseChineseInteger parses a place-value number (一千二百三十四). It rejects
// sequences that are not a number rather than guessing: 千方百计 must not become
// 10000000百计.
func parseChineseInteger(run []rune) (int64, bool) {
	var total, section, number int64
	var lastUnit int
	var lastSmallUnit int
	previousWasDigit := false
	previousWasUnit := false
	// A 零 marks a skipped place: 一百零二 is 102, not 120. It also means the
	// digit after it is a plain addend rather than a colloquial trailing digit.
	zeroSeen := false

	for _, r := range run {
		if r == '零' || r == '〇' || r == '○' {
			zeroSeen = true
			number = 0
			previousWasDigit = false
			previousWasUnit = false
			continue
		}
		if digit, ok := chineseDigit[r]; ok {
			// Two digits in a row is not a place-value number. A digit-only run
			// is handled before this function, so reaching here means a mix.
			// 零 is handled above, so it does not count as a digit here.
			if previousWasDigit {
				return 0, false
			}
			number = int64(digit)
			previousWasDigit = true
			previousWasUnit = false
			continue
		}
		unit, ok := chineseUnit[r]
		if !ok {
			return 0, false
		}
		// A small unit cannot follow a unit directly (千百 is not a number).
		// A group unit may follow a small unit (十万, 百万, 千万), and group
		// units must increase (万亿 is possible, 亿万 is not).
		if previousWasUnit {
			if unit < tenThousand {
				return 0, false
			}
			if lastUnit >= tenThousand && unit <= lastUnit {
				return 0, false
			}
		}
		switch unit {
		case tenThousand, hundredMillion:
			var ok bool
			if section, ok = addInt64(section, number); !ok {
				return 0, false
			}
			if section == 0 && total == 0 {
				// A bare 万 or 亿 is a word, not a number. 一万亿 is a number:
				// the 亿 multiplies the 万 section that came before it.
				return 0, false
			}
			if unit == hundredMillion {
				combined, ok := addInt64(total, section)
				if !ok {
					return 0, false
				}
				if total, ok = mulInt64(combined, hundredMillion); !ok {
					return 0, false
				}
			} else {
				scaled, ok := mulInt64(section, tenThousand)
				if !ok {
					return 0, false
				}
				if total, ok = addInt64(total, scaled); !ok {
					return 0, false
				}
			}
			section = 0
			number = 0
			lastUnit = unit
			lastSmallUnit = 0
		default:
			// Small units inside one section must strictly decrease:
			// 一百二千 is not a number. Without this, a malformed decode
			// would be read as 2100.
			if lastSmallUnit != 0 && unit >= lastSmallUnit {
				return 0, false
			}
			if number == 0 {
				// 十二 and 一百: an omitted one before the first unit.
				number = 1
			}
			product, ok := mulInt64(number, int64(unit))
			if !ok {
				return 0, false
			}
			if section, ok = addInt64(section, product); !ok {
				return 0, false
			}
			number = 0
			lastUnit = unit
			lastSmallUnit = unit
		}
		previousWasDigit = false
		previousWasUnit = true
	}

	if number > 0 {
		// Colloquial trailing digits: 一千二 is 1200, 三万五 is 35000. After a
		// 零 the digit is an exact place: 一百零二 is 102.
		var addend int64
		switch {
		case zeroSeen:
			addend = number
		case lastUnit >= 100:
			addend = number * int64(lastUnit/10)
		default:
			addend = number
		}
		var ok bool
		if section, ok = addInt64(section, addend); !ok {
			return 0, false
		}
	}
	if section == 0 && total == 0 {
		return 0, false
	}
	sum, ok := addInt64(total, section)
	if !ok {
		return 0, false
	}
	return sum, true
}

func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 {
		return 0, false
	}
	product := a * b
	if product/b != a {
		return 0, false
	}
	return product, true
}

// exceptionAt returns the length of a fixed expression that starts at index, or
// 0. The expressions are the numeral-containing words and idioms that a numeric
// reading would destroy.
func exceptionAt(runes []rune, index int) int {
	for _, phrase := range numeralExceptions[runes[index]] {
		if !hasRunePrefix(runes, index, phrase) {
			continue
		}
		end := index + len(phrase)
		if isNumeral(phrase[len(phrase)-1]) && end < len(runes) && isNumeral(runes[end]) {
			// The exception ends in a numeral and another numeral follows.
			// Two cases:
			//   - the exception is all numerals (三八 in 三八零零): it is inside
			//     a longer digit string, so let the numeral run handle it;
			//   - the exception contains a word (唯一 in 唯一一家): the next
			//     numeral belongs to the phrase's object, and converting it
			//     would produce 唯11家. Keep the run with the exception.
			if allNumeral(phrase) {
				continue
			}
			if runEnd := numeralRunEnd(runes, end); runEnd > end {
				return runEnd - index
			}
		}
		return len(phrase)
	}
	return 0
}

func allNumeral(runes []rune) bool {
	for _, r := range runes {
		if !isNumeral(r) {
			return false
		}
	}
	return true
}

// hasPrefix compares a string prefix against runes without converting the
// prefix to a new slice. The normalizer checks many short prefixes per numeral,
// and this is the hot part of a preedit update.
func hasPrefix(runes []rune, index int, prefix string) bool {
	if index < 0 {
		return false
	}
	for _, r := range prefix {
		if index >= len(runes) || runes[index] != r {
			return false
		}
		index++
	}
	return true
}

func hasRunePrefix(runes []rune, index int, prefix []rune) bool {
	if index < 0 || index+len(prefix) > len(runes) {
		return false
	}
	for offset, r := range prefix {
		if runes[index+offset] != r {
			return false
		}
	}
	return true
}

// followedByMeasureWord reports whether a measure word or a unit follows end.
// Multi-character measures are checked before single ones, so 小时 is not read as
// the single character 时.
func followedByMeasureWord(runes []rune, end int) bool {
	for _, measure := range measureWords {
		if hasPrefix(runes, end, measure) {
			return true
		}
	}
	return false
}

// atBoundary reports whether end is the end of the text or punctuation.
func atBoundary(runes []rune, end int) bool {
	if end >= len(runes) {
		return true
	}
	return isSentenceBoundary(runes[end]) || unicode.IsSpace(runes[end])
}

func isSentenceBoundary(r rune) bool {
	switch r {
	case '。', '，', '、', '；', '：', '！', '？', '…', '—', '·',
		'.', ',', ';', ':', '!', '?', '(', ')', '（', '）',
		'「', '」', '『', '』', '“', '”', '‘', '’', '"', '\'':
		return true
	}
	return false
}
