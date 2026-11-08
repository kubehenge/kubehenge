package templates

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// factsMarker starts the line of a template's description that records what its build installed.
const factsMarker = "kubehenge-build:"

// factsCommand prints a finished node's running kernel and, on the next line, a SHA-256 of its
// installed packages with their versions.
const factsCommand = `uname -r && rpm -qa --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}\n' | LC_ALL=C sort | sha256sum | cut -d' ' -f1`

// BuildFacts is what a node image build installed: the kernel it boots and a hash of every package.
// A mirror one sync behind gives a build named newer than the last whose kernel is older, and the
// hash tells two builds of the same kernel apart.
type BuildFacts struct {
	Kernel, Packages string
}

// parseFactsOutput reads factsCommand's output.
func parseFactsOutput(out string) (BuildFacts, error) {
	lines := strings.Fields(out)
	if len(lines) != 2 || len(lines[1]) != 64 {
		return BuildFacts{}, fmt.Errorf("the build's kernel and package hash: expected two values, got %q", strings.TrimSpace(out))
	}
	return BuildFacts{Kernel: lines[0], Packages: lines[1]}, nil
}

// Description is the text of a template's description that carries the facts, after its fixed text.
func (f BuildFacts) Description(text string) string {
	return fmt.Sprintf("%s\n%s kernel=%s packages=%s", text, factsMarker, f.Kernel, f.Packages)
}

// ParseBuildFacts reads the facts out of a template's description. It is false for a template that
// records none, such as one built before the facts were recorded.
func ParseBuildFacts(description string) (BuildFacts, bool) {
	for line := range strings.Lines(description) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), factsMarker)
		if !ok {
			continue
		}
		var f BuildFacts
		for field := range strings.FieldsSeq(rest) {
			if v, ok := strings.CutPrefix(field, "kernel="); ok {
				f.Kernel = v
			}
			if v, ok := strings.CutPrefix(field, "packages="); ok {
				f.Packages = v
			}
		}
		return f, f.Kernel != ""
	}
	return BuildFacts{}, false
}

// OlderThan tells whether the build's kernel is older than the previous build's.
func (f BuildFacts) OlderThan(previous BuildFacts) bool {
	return CompareKernels(f.Kernel, previous.Kernel) < 0
}

// OlderBuilds tells which of a line's builds, given as their facts oldest first, have a kernel older
// than the newest earlier build that is not itself older. A build without facts (nil) is never
// older and sets no baseline.
func OlderBuilds(facts []*BuildFacts) []bool {
	older := make([]bool, len(facts))
	var baseline *BuildFacts
	for i, f := range facts {
		switch {
		case f == nil:
		case baseline != nil && f.OlderThan(*baseline):
			older[i] = true
		default:
			baseline = f
		}
	}
	return older
}

// CompareKernels orders two kernel release strings as rpm orders versions: runs of digits by value,
// runs of letters by spelling, a number above a word, and punctuation only as a separator.
func CompareKernels(a, b string) int {
	for a != "" || b != "" {
		a, b = strings.TrimLeftFunc(a, isSeparator), strings.TrimLeftFunc(b, isSeparator)
		if a == "" || b == "" {
			break
		}
		numeric := unicode.IsDigit(rune(a[0]))
		var segA, segB string
		segA, a = takeSegment(a, numeric)
		segB, b = takeSegment(b, numeric)
		if segB == "" {
			// The other side has a word where this one has a number, or the reverse.
			if numeric {
				return 1
			}
			return -1
		}
		if numeric {
			if c := compareNumbers(segA, segB); c != 0 {
				return c
			}
		} else if c := strings.Compare(segA, segB); c != 0 {
			return c
		}
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	}
	return 1
}

func isSeparator(r rune) bool { return !unicode.IsDigit(r) && !unicode.IsLetter(r) }

// takeSegment splits the leading run of digits or letters off s.
func takeSegment(s string, numeric bool) (string, string) {
	end := 0
	for end < len(s) && ((numeric && unicode.IsDigit(rune(s[end]))) || (!numeric && unicode.IsLetter(rune(s[end])))) {
		end++
	}
	return s[:end], s[end:]
}

func compareNumbers(a, b string) int {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		// Longer than a machine word: compare as text without leading zeros.
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	}
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
