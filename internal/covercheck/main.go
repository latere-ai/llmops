// Command covercheck gates coverage per package rather than as a
// repository average.
//
// An average lets a well-tested package carry an untested one and
// reports a number nobody can act on. That is not hypothetical here:
// when this was written the repository sat at 90.4% and passed, while
// internal/harness was at 85.7% and internal/install at 87.8% — both
// below the floor, both invisible behind the average.
//
// Exemptions live in code with a reason. A package exempted without one
// is a package nobody decided to exempt.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// threshold is the floor every package clears.
const threshold = 90.0

// exempt maps a package suffix to why it does not have to clear the
// floor. Keep this short; each entry is a decision, not a shortcut.
var exempt = map[string]string{
	"internal/covercheck": "main() is flag parsing and os.Exit around run(), " +
		"which is tested directly; the rest of the package is covered",
}

type counts struct{ covered, total int }

func main() {
	profile := flag.String("profile", "coverage.out", "coverage profile to read")
	flag.Parse()
	if err := run(*profile, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "covercheck:", err)
		os.Exit(1)
	}
}

func run(profile string, out io.Writer) error {
	byPkg, err := parse(profile)
	if err != nil {
		return err
	}
	if len(byPkg) == 0 {
		return fmt.Errorf("%s covers no packages", profile)
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var failed []string
	for _, p := range pkgs {
		c := byPkg[p]
		if c.total == 0 {
			continue // no statements: nothing to cover
		}
		pct := 100 * float64(c.covered) / float64(c.total)
		if why, ok := exemptFor(p); ok {
			fmt.Fprintf(out, "  %-40s %6.1f%%  exempt: %s\n", short(p), pct, why)
			continue
		}
		mark := " "
		if pct < threshold {
			mark = "!"
			failed = append(failed, fmt.Sprintf("%s %.1f%%", short(p), pct))
		}
		fmt.Fprintf(out, "%s %-40s %6.1f%%\n", mark, short(p), pct)
	}
	if len(failed) > 0 {
		return fmt.Errorf("below %.0f%%: %s", threshold, strings.Join(failed, ", "))
	}
	fmt.Fprintf(out, "\nevery package clears %.0f%%\n", threshold)
	return nil
}

// parse sums statement counts per package. A block can appear once per
// test binary that executed it, so the same block is counted once and
// marked covered if any run covered it.
func parse(profile string) (map[string]*counts, error) {
	f, err := os.Open(profile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type blockKey struct{ file, span string }
	blocks := map[blockKey]struct {
		pkg     string
		stmts   int
		covered bool
	}{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// path/file.go:start.col,end.col numStmts count
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		file, rest := line[:colon], line[colon+1:]
		fields := strings.Fields(rest)
		if len(fields) != 3 {
			continue
		}
		stmts, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		k := blockKey{file, fields[0]}
		b := blocks[k]
		b.pkg, b.stmts = path.Dir(file), stmts
		b.covered = b.covered || count > 0
		blocks[k] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	byPkg := map[string]*counts{}
	for _, b := range blocks {
		c := byPkg[b.pkg]
		if c == nil {
			c = &counts{}
			byPkg[b.pkg] = c
		}
		c.total += b.stmts
		if b.covered {
			c.covered += b.stmts
		}
	}
	return byPkg, nil
}

func exemptFor(pkg string) (string, bool) {
	for suffix, why := range exempt {
		if strings.HasSuffix(pkg, suffix) {
			return why, true
		}
	}
	return "", false
}

// short drops the module prefix so the output is readable.
func short(pkg string) string {
	if _, after, ok := strings.Cut(pkg, "llmops/"); ok {
		return after
	}
	return pkg
}
