// Spec seeding (CORP-01): the corpus oracle starts life from the app's own
// Playwright specs — human-authored test code, so seeded behaviors are
// trusted and land already verified. Seed walks the app's configured specs
// dir (config key corpus_specs_dir, default DefaultSpecsDir) in the app repo
// at a fixed commit, parses test/describe titles into deterministic behavior
// keys, and upserts them by (app_id, key): re-seeding the same repo state
// never duplicates or churns rows. Seed never deletes: behaviors for specs
// removed upstream stay in the oracle (the human verify path marks them
// stale, not seed).
package corpus

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultSpecsDir is the corpus_specs_dir default — bet-client's Playwright
// e2e tree. T4 wires the per-app config key; apps without an override use
// this.
const DefaultSpecsDir = "apps/bet-client/e2e/"

// BehaviorKeyPrefix namespaces spec-seeded behavior keys: PWSPEC-<TESTNAME>.
const BehaviorKeyPrefix = "PWSPEC"

// SeedSummary reports one seed pass.
type SeedSummary struct {
	FilesScanned int // .spec.ts files fetched and parsed
	Behaviors    int // upserts performed (idempotent across re-runs)
}

// SeedSpecs fetches *.spec.ts files under dir at commit (branch or SHA) and
// upserts one verified behavior per parsed test, provenance pinning the spec
// path and commit. Spec fetch failures are hard errors: a partially-seeded
// oracle is worse than no seed.
func SeedSpecs(ctx context.Context, pool *pgxpool.Pool, client *github.Client, appID pgtype.UUID, owner, repo, dir, commit string) (SeedSummary, error) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultSpecsDir
	}
	paths, err := walkSpecs(ctx, client, owner, repo, dir, commit)
	if err != nil {
		return SeedSummary{}, err
	}
	summary := SeedSummary{}
	for _, path := range paths {
		content, err := client.GetFile(ctx, owner, repo, commit, path)
		if err != nil {
			return summary, fmt.Errorf("corpus: seed fetch %s@%s: %w", path, commit, err)
		}
		summary.FilesScanned++
		for _, t := range parseSpec(content) {
			title := t.Title()
			if title == "" {
				continue
			}
			if err := UpsertBehavior(ctx, pool, &Behavior{
				AppID:       appID,
				Key:         behaviorKey(title),
				Title:       title,
				Description: behaviorDescription(t),
				Status:      BehaviorVerified,
				Provenance: Provenance{
					Kind:   ProvenancePlaywrightSpec,
					Path:   path,
					Commit: commit,
				},
			}); err != nil {
				return summary, err
			}
			summary.Behaviors++
		}
	}
	return summary, nil
}

// walkSpecs BFS-walks dir via the contents API, returning every .spec.ts
// file path in deterministic (sorted) order. The contents API is one level
// deep per call, so the walk recurses into subdirectories.
func walkSpecs(ctx context.Context, client *github.Client, owner, repo, dir, commit string) ([]string, error) {
	var specs []string
	pending := []string{dir}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		entries, err := client.ListDir(ctx, owner, repo, commit, current)
		if err != nil {
			return nil, fmt.Errorf("corpus: seed list %s@%s: %w", current, commit, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		for _, e := range entries {
			switch e.Type {
			case "dir":
				pending = append(pending, e.Path)
			case "file":
				if strings.HasSuffix(e.Name, ".spec.ts") {
					specs = append(specs, e.Path)
				}
			}
		}
	}
	return specs, nil
}

// SpecTest is one parsed Playwright test: its describe chain, its own title,
// and the expect(...) summaries inside its body — enough to write a
// falsifiable corpus description without a TypeScript grammar.
type SpecTest struct {
	Describes []string // outermost first
	Name      string   // the test(...) title itself
	Expects   []string // expect(<subject>) texts, in source order
}

// Title renders the full path: "Describe > Nested > test name".
func (t SpecTest) Title() string {
	parts := append(append([]string{}, t.Describes...), t.Name)
	return strings.Join(parts, " > ")
}

// behaviorDescription builds the falsifiable expected-behavior text:
// "<title path> — expects: expect(a).toBeVisible(); expect(b)...". A test
// with no expects still seeds (its title is the assertion).
func behaviorDescription(t SpecTest) string {
	desc := t.Title()
	if len(t.Expects) > 0 {
		desc += " — expects: " + strings.Join(t.Expects, "; ")
	}
	return desc
}

// behaviorKey derives PWSPEC-<TESTNAME> from the full title path: uppercase
// slug of [describe...] > test title. Deterministic — the same spec text
// always yields the same key, which is what makes seed idempotent.
func behaviorKey(title string) string {
	lower := strings.ToLower(title)
	var b strings.Builder
	prevDash := true // trim leading dashes
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteRune('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	const maxSlug = 64
	if len(slug) > maxSlug {
		slug = strings.TrimSuffix(slug[:maxSlug], "-")
	}
	return BehaviorKeyPrefix + "-" + strings.ToUpper(slug)
}

// matcher finds block openers (describe / test) with a quoted first
// argument. Quote char is captured so titles containing the other quotes
// still match; escaped chars are tolerated inside the title.
var blockRe = regexp.MustCompile(`\b(test|describe)((?:\.\w+)*)\s*\(\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|` + "`((?:[^`\\\\]|\\\\.)*)`" + `)`)

// expectRe finds expect(...) call starts; the argument is captured by
// scanning balanced parens from the match.
var expectRe = regexp.MustCompile(`\bexpect\s*\(`)

// parseSpec extracts the tests of one spec file. It is a deterministic,
// grammar-free scan (titles come from string literals only): block extents
// are computed by balanced-paren counting from each opener, so nesting is
// handled without tracking TS syntax beyond strings and brackets.
func parseSpec(content string) []SpecTest {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var (
		tests     []SpecTest
		describes []struct {
			start, end int
			title      string
		}
		matches []struct {
			kind, title string
			start, end  int // extent of the whole call (open paren to its closing paren)
		}
	)
	for _, m := range blockRe.FindAllStringSubmatchIndex(content, -1) {
		kind := content[m[2]:m[3]]
		if strings.Contains(content[m[4]:m[5]], "describe") {
			kind = "describe" // test.describe(...) opens a describe scope
		}
		title, _ := firstGroup(content, m, 6, 7, 8, 9, 10, 11)
		_, close_, ok := balancedSpan(content, m[1])
		if !ok {
			continue
		}
		matches = append(matches, struct {
			kind, title string
			start, end  int
		}{kind, title, m[0], close_})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	// Describes get extents first; tests are matched to enclosing describes.
	for _, m := range matches {
		if m.kind == "describe" {
			describes = append(describes, struct {
				start, end int
				title      string
			}{m.start, m.end, m.title})
		}
	}
	for _, m := range matches {
		if m.kind != "test" {
			continue
		}
		t := SpecTest{Name: m.title}
		for _, d := range describes {
			// Enclosing describes strictly contain the test's call span.
			if d.start < m.start && m.end < d.end {
				t.Describes = append(t.Describes, d.title)
			}
		}
		for _, loc := range expectRe.FindAllStringIndex(content[m.start:m.end], -1) {
			// loc[1]-1 is the '(' the regex consumed; expectText needs the offset.
			if text, ok := expectText(content, m.start+loc[1]-1); ok {
				t.Expects = append(t.Expects, text)
			}
		}
		tests = append(tests, t)
	}
	return tests
}

// balancedSpan returns the span of the balanced parens opened at or after
// the byte offset from (i.e. the call's argument list), skipping string
// literals so parens inside titles/bodies do not confuse the counter.
func balancedSpan(s string, from int) (open, close_ int, ok bool) {
	// The opener was consumed by the block regex (from sits right after
	// it), so accounting starts at depth 1: the matching close is found
	// when the depth returns to zero.
	depth := 1
	i := from
	inStr := byte(0)
	for ; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = c
		case '(', '[':
			depth++
		case ')', ']':
			depth--
			if depth == 0 {
				return from, i, true
			}
		}
	}
	return 0, 0, false
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// balancedParen returns the index of the ')' matching the '(' at off,
// skipping string literals so parens inside titles/messages do not confuse
// the counter.
func balancedParen(s string, off int) (int, bool) {
	depth := 0
	inStr := byte(0)
	for i := off; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// expectText renders one expect(...) call as falsifiable text:
// "expect(<subject>)" plus any method calls chained directly after it, e.g.
// "expect(page.getByTestId('x')).toBeVisible()". A Playwright message
// argument ("expect(loc, 'never visible')") is dropped — only the subject
// and the assertion chain survive.
func expectText(s string, off int) (string, bool) {
	if off >= len(s) || s[off] != '(' {
		return "", false
	}
	end, ok := balancedParen(s, off)
	if !ok {
		return "", false
	}
	subject := strings.TrimSpace(strings.SplitN(s[off+1:end], ",", 2)[0])
	var b strings.Builder
	b.WriteString("expect(" + subject + ")")
	i := end + 1
	for i < len(s) && s[i] == '.' {
		j := i + 1
		for j < len(s) && (s[j] == '_' || isWordByte(s[j])) {
			j++
		}
		k := j
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k >= len(s) || s[k] != '(' {
			break
		}
		c, ok2 := balancedParen(s, k)
		if !ok2 {
			break
		}
		b.WriteString(s[i:j] + s[k:c+1])
		i = c + 1
	}
	return b.String(), true
}

func parenArg(s string, off int) string {
	if off >= len(s) || s[off] != '(' {
		return ""
	}
	depth := 0
	inStr := byte(0)
	for i := off; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			switch c {
			case '\\':
				i++
			case inStr:
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(strings.SplitN(s[off+1:i], ",", 2)[0])
			}
		}
	}
	return ""
}

// firstGroup returns the first non-empty capture among the given pair
// indices of a FindAllStringSubmatchIndex result.
func firstGroup(s string, m []int, pairs ...int) (string, bool) {
	for i := 0; i+1 < len(pairs); i += 2 {
		a, b := m[pairs[i]], m[pairs[i+1]]
		if a >= 0 && b >= 0 {
			return s[a:b], true
		}
	}
	return "", false
}
