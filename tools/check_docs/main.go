// Command check_docs verifies the documentation's cross-references.
//
// Every relative markdown link in README.md and docs/*.md must resolve: the
// target file must exist, and if the link carries an anchor, the anchor must
// be a heading in the target file (GitHub-style slug, with -1/-2 suffixes for
// duplicate headings). External links (http, https, mailto) are skipped.
//
// It is part of `make check`: the docs cross-reference each other densely and
// an agent reads them the way it reads code, so a rotted reference is a
// silent failure rather than a cosmetic one. It lives under tools/ rather
// than cmd/ on purpose - it is build tooling, so it ships in nothing and the
// unreachable-code check, which covers cmd/ and internal/, never reaches it.
// Go rather than python3: a machine that can build this project has the
// toolchain by definition, while python3 is not a build prerequisite.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

var (
	// A markdown heading outside a code fence: at most six #, a space, the text.
	headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	// The target of a markdown link.
	linkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	// A link embedded in a heading, reduced to its text for the slug.
	linkInHeadingRE = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
)

// isFenceEdge reports whether the line opens or closes a fenced code block.
func isFenceEdge(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// slugify produces the GitHub-style anchor for a heading: lowercase,
// punctuation dropped, spaces to hyphens (so "D22 — One" is d22--one).
func slugify(heading string) string {
	text := linkInHeadingRE.ReplaceAllString(heading, "$1")
	text = strings.NewReplacer("*", "", "_", "", "`", "", "~", "").Replace(text)
	text = strings.ToLower(text)
	var b strings.Builder
	for _, ch := range text {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == ' ' || ch == '-' || ch == '_' {
			b.WriteRune(ch)
		}
	}
	return strings.ReplaceAll(b.String(), " ", "-")
}

// slugsOf returns the anchor slugs of a file's headings, in order; duplicate
// headings are suffixed -1, -2 as GitHub does. Fenced code blocks are skipped.
func slugsOf(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("cannot read %s: %v", path, err)
	}
	var (
		counts  = map[string]int{}
		slugs   []string
		inFence bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		if isFenceEdge(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := headingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		slug := slugify(m[2])
		counts[slug]++
		n := counts[slug]
		if n == 1 {
			slugs = append(slugs, slug)
		} else {
			slugs = append(slugs, fmt.Sprintf("%s-%d", slug, n-1))
		}
	}
	return slugs
}

// findRoot walks up from the working directory to the checkout: the
// directory that holds both README.md and docs/.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isFile(filepath.Join(dir, "README.md")) && isDir(filepath.Join(dir, "docs")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no repository root (README.md plus docs/) at or above %s", dir)
		}
		dir = parent
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// rel displays path relative to root, or the absolute path when they are not
// related.
func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docs-check: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	root, err := findRoot()
	if err != nil {
		fatalf("%v", err)
	}

	docs := []string{filepath.Join(root, "README.md")}
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		fatalf("%v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		docs = append(docs, filepath.Join(root, "docs", n))
	}

	slugs := map[string][]string{}
	for _, doc := range docs {
		if isFile(doc) {
			slugs[filepath.Base(doc)] = slugsOf(doc)
		}
	}

	problems := 0
	anchorsChecked := 0
	for _, doc := range docs {
		if !isFile(doc) {
			fmt.Printf("docs-check: missing documentation file: %s\n", rel(root, doc))
			problems++
			continue
		}
		data, err := os.ReadFile(doc)
		if err != nil {
			fatalf("%v", err)
		}
		inFence := false
		for i, line := range strings.Split(string(data), "\n") {
			if isFenceEdge(line) {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range linkRE.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				filePart, anchor, _ := strings.Cut(target, "#")
				base := doc
				if filePart != "" {
					base = filepath.Join(filepath.Dir(doc), filePart)
				}
				if !isFile(base) {
					fmt.Printf("%s:%d: link target not found: %s\n", rel(root, doc), i+1, target)
					problems++
					continue
				}
				if anchor == "" {
					continue
				}
				anchorsChecked++
				if !slices.Contains(slugs[filepath.Base(base)], anchor) {
					fmt.Printf("%s:%d: anchor not in %s: #%s\n", rel(root, doc), i+1, filepath.Base(base), anchor)
					problems++
				}
			}
		}
	}

	if problems > 0 {
		fmt.Printf("docs-check: %d broken reference(s)\n", problems)
		os.Exit(1)
	}
	fmt.Printf("docs-check: %d files, %d anchors checked, all relative links and anchors resolve\n", len(docs), anchorsChecked)
}
