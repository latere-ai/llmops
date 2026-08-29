// Package speclint checks that the spec tree describes itself
// consistently.
//
// It exists because the index drifted badly: on 2026-08-29 every row in
// specs/README.md read `draft`, including five specs that were built,
// deployed and serving. It had been hand-edited a dozen times that same
// day. A status column that disagrees with the code is worse than no
// column, because a reader trusts it.
package speclint

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The whole vocabulary. Three values, and the difference between them is
// meant to be legible from the word alone:
//
//	draft    — nothing of it is in the product
//	partial  — in the product, a named criterion still open
//	complete — in the product, every criterion holds
var allowed = map[string]bool{"draft": true, "partial": true, "complete": true}

const specsDir = "../../specs"

var (
	statusRe = regexp.MustCompile(`(?m)^status: *(.+)$`)
	titleRe  = regexp.MustCompile(`(?m)^title: *(.+)$`)
	// | 019 | [Name](019-file.md) | status | scope |
	rowRe = regexp.MustCompile(`(?m)^\| *([0-9]{3}[a-z]?) *\| *\[[^\]]*\]\(([^)]+)\) *\| *([^|]+?) *\|`)
)

func specFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(specsDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range all {
		if filepath.Base(p) != "README.md" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatal("no specs found")
	}
	sort.Strings(out)
	return out
}

func frontmatterStatus(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Only the frontmatter block, so prose mentioning "status:" cannot
	// be mistaken for the field.
	body := string(data)
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("%s: no frontmatter", filepath.Base(path))
	}
	end := strings.Index(body[4:], "\n---")
	if end < 0 {
		t.Fatalf("%s: unterminated frontmatter", filepath.Base(path))
	}
	m := statusRe.FindStringSubmatch(body[:end+4])
	if m == nil {
		t.Fatalf("%s: frontmatter has no status", filepath.Base(path))
	}
	return strings.TrimSpace(m[1])
}

func TestStatusVocabularyIsClosed(t *testing.T) {
	for _, p := range specFiles(t) {
		got := frontmatterStatus(t, p)
		if !allowed[got] {
			t.Errorf("%s: status %q is not one of draft|partial|complete",
				filepath.Base(p), got)
		}
	}
}

func TestEverySpecHasATitle(t *testing.T) {
	for _, p := range specFiles(t) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !titleRe.Match(data) {
			t.Errorf("%s: frontmatter has no title", filepath.Base(p))
		}
	}
}

// TestIndexAgreesWithTheSpecs is the check that would have caught the
// drift: the index's status column must equal the spec's own
// frontmatter, for every row.
func TestIndexAgreesWithTheSpecs(t *testing.T) {
	index, err := os.ReadFile(filepath.Join(specsDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := rowRe.FindAllStringSubmatch(string(index), -1)
	if len(rows) == 0 {
		t.Fatal("no spec rows found in the index; has the table shape changed?")
	}

	seen := map[string]bool{}
	for _, r := range rows {
		num, file, status := r[1], r[2], strings.TrimSpace(r[3])
		// Strip any emphasis someone added to make a row stand out.
		status = strings.Trim(status, "*_`")
		path := filepath.Join(specsDir, file)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("index row %s links to %s, which does not exist", num, file)
			continue
		}
		seen[file] = true
		if want := frontmatterStatus(t, path); status != want {
			t.Errorf("index says %s is %q; %s says %q",
				num, status, file, want)
		}
	}

	// Every spec must appear. A spec absent from the index is invisible.
	for _, p := range specFiles(t) {
		if !seen[filepath.Base(p)] {
			t.Errorf("%s is not listed in specs/README.md", filepath.Base(p))
		}
	}
}

// TestEveryWikilinkResolves pins the cross-references. These were
// checked by hand all through the GB10 track, which is exactly the work
// that stops happening.
func TestEveryWikilinkResolves(t *testing.T) {
	linkRe := regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	for _, p := range specFiles(t) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(data), -1) {
			target := filepath.Join(specsDir, m[1]+".md")
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: [[%s]] resolves to nothing", filepath.Base(p), m[1])
			}
		}
	}
}
