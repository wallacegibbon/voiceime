package textnorm

import (
	_ "embed"
	"sort"
	"strings"
)

// The two dictionaries are OpenCC's Traditional-to-Simplified data, copied
// verbatim (see data/NOTICE). They are plain tab-separated tables: traditional
// text, a tab, then one or more simplified candidates separated by spaces. The
// first candidate is the one OpenCC itself chooses.
//
// They are embedded rather than read from disk because the binary has to work on
// a machine with no OpenCC installed, and because loading them from a path that
// happened to exist on the build machine is the kind of dependency that only
// fails after a deploy.
//
//go:embed data/TSCharacters.txt
var tsCharactersData string

//go:embed data/TSPhrases.txt
var tsPhrasesData string

// converter performs OpenCC's t2s conversion: a greedy longest-phrase match over
// TSPhrases, falling back to the character table.
//
// It is a reimplementation rather than a dependency on the Go OpenCC port because
// the port embeds every conversion (1.4 MB) to use one of them. These two tables
// are 39 KB and the matcher is the loop below.
type converter struct {
	characters map[rune]string
	phrases    map[string]string
	// phraseLengths is indexed by the phrase's first rune, longest first. It is
	// what keeps ordinary text cheap: a rune that starts no phrase never enters
	// the length loop, so most of a sentence costs one map lookup.
	phraseLengths map[rune][]int
}

var traditionalToSimplified = newConverter(tsCharactersData, tsPhrasesData)

func newConverter(characters, phrases string) *converter {
	converted := &converter{
		characters:    make(map[rune]string, 4096),
		phrases:       make(map[string]string, 512),
		phraseLengths: make(map[rune][]int, 256),
	}
	for _, line := range strings.Split(characters, "\n") {
		source, target, ok := splitDictionaryLine(line)
		if !ok {
			continue
		}
		runes := []rune(source)
		if len(runes) == 1 {
			converted.characters[runes[0]] = target
		}
	}
	for _, line := range strings.Split(phrases, "\n") {
		source, target, ok := splitDictionaryLine(line)
		if !ok {
			continue
		}
		runes := []rune(source)
		converted.phrases[source] = target
		converted.phraseLengths[runes[0]] = append(converted.phraseLengths[runes[0]], len(runes))
	}
	for first, lengths := range converted.phraseLengths {
		sort.Sort(sort.Reverse(sort.IntSlice(lengths)))
		unique := lengths[:0]
		for _, length := range lengths {
			if len(unique) == 0 || unique[len(unique)-1] != length {
				unique = append(unique, length)
			}
		}
		converted.phraseLengths[first] = unique
	}
	return converted
}

// splitDictionaryLine parses one table row. A row without a tab is a comment or
// the trailing empty line, and is skipped rather than treated as a mapping.
func splitDictionaryLine(line string) (source, target string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	source, rest, found := strings.Cut(line, "\t")
	if !found || source == "" {
		return "", "", false
	}
	candidates := strings.Fields(rest)
	if len(candidates) == 0 {
		return "", "", false
	}
	return source, candidates[0], true
}

// convert rewrites Traditional characters and phrases as Simplified. It is a
// single pass over the input, in OpenCC's longest-phrase-first order. It is not
// idempotent: the t2s data deliberately preserves some forms that it would
// convert if they were the input (乾圖 becomes 乾图, and converting 乾图 again
// gives 干图). Callers must therefore normalize raw recognizer text, not text
// that has already been stored in normalized form.
func (c *converter) convert(text string) string {
	if text == "" {
		return text
	}
	runes := []rune(text)
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(runes); {
		matched := false
		// Longest match first: OpenCC's phrase dictionary contains entries whose
		// prefix is also a phrase, and the longer one is the correct reading.
		for _, length := range c.phraseLengths[runes[index]] {
			if length > len(runes)-index {
				continue
			}
			if replacement, found := c.phrases[string(runes[index:index+length])]; found {
				out.WriteString(replacement)
				index += length
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if replacement, found := c.characters[runes[index]]; found {
			out.WriteString(replacement)
		} else {
			out.WriteRune(runes[index])
		}
		index++
	}
	return out.String()
}
