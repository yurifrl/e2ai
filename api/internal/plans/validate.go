// validate.go applies the lookup-dependent plan rules (PLAN-01): the app must
// exist and the frontmatter surfaces must be its subset, `uses:` keys must
// resolve to verified corpus behaviors, and derived plans must carry real
// provenance (the tautology guard, DERV-03). Lookups go through narrow
// interfaces so validation stays pure — callers wire pgx-backed implementors.
package plans

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gobwas/glob"
)

// AppLookup resolves a frontmatter app ref to the registered app's surfaces
// (registered_apps.surfaces). ok=false means the ref matches no app.
type AppLookup interface {
	LookupApp(ref string) (surfaces []string, ok bool)
}

// BehaviorStatus resolves a corpus behavior key to its review status.
// ok=false means the key does not exist; a non-verified status rejects the
// plan (verified-only rule, CORP-01).
type BehaviorStatus interface {
	BehaviorStatus(key string) (status string, ok bool)
}

// diffSource identifies the PR-diff provenance in a derived plan's sources
// list ("diff" bare, or namespaced like "diff:PR-123").
const DiffSource = "diff"

// IsDiffSource reports whether a provenance id is diff-derived ("diff" or
// "diff:*"). Derivation callers use it to prove their drafts never carry a
// diff-only sources list (the tautology guard, DERV-03).
func IsDiffSource(id string) bool {
	return id == DiffSource || strings.HasPrefix(id, DiffSource+":")
}

// Validate checks a parsed plan against the environment's lookups. It
// accumulates nothing: the first failing rule returns, with a message that
// names the offending field or step.
func Validate(plan *Plan, apps AppLookup, behaviors BehaviorStatus) error {
	fm := plan.Frontmatter
	surfaces, ok := apps.LookupApp(fm.App)
	if !ok {
		return fmt.Errorf("app %q: not registered", fm.App)
	}
	known := map[string]bool{}
	for _, s := range surfaces {
		known[s] = true
	}
	for _, s := range fm.Surfaces {
		if !known[s] {
			return fmt.Errorf("surface %q: not a surface of app %q", s, fm.App)
		}
	}
	if err := validatePRFilter(fm); err != nil {
		return err
	}
	if err := validateTautology(fm); err != nil {
		return err
	}
	for _, step := range plan.Steps {
		if err := validateStep(step, behaviors); err != nil {
			return err
		}
	}
	return nil
}

// validatePRFilter compiles every glob so a malformed pattern (for example an
// unbalanced `[`) fails at ingest rather than mid-derivation.
func validatePRFilter(fm Frontmatter) error {
	if fm.PRFilter == nil {
		return nil
	}
	for _, p := range fm.PRFilter.Paths {
		if _, err := glob.Compile(p); err != nil {
			return fmt.Errorf("pr-filter paths: glob %q: %w", p, err)
		}
	}
	return nil
}

// validateTautology enforces the derived-plan provenance rule (DERV-03):
// a derived plan's sources must be non-empty and not entirely diff-derived —
// a diff-only plan re-derives its own conclusion (the tautology guard kills
// these at ingest). Human-authored plans carry no `derived:` and skip this.
func validateTautology(fm Frontmatter) error {
	if !fm.Derived {
		return nil
	}
	if len(fm.Sources) == 0 {
		return fmt.Errorf("derived plan: sources required (provenance ids; %q alone is a tautology)", DiffSource)
	}
	for _, id := range fm.Sources {
		if !IsDiffSource(id) {
			return nil
		}
	}
	return fmt.Errorf("derived plan: all sources diff-derived (%v): tautological, need an independent source (corpus or seeded run)", fm.Sources)
}

// validateStep checks one step: judge present and falsifiable, `uses:` keys
// resolving to verified behaviors.
func validateStep(step Step, behaviors BehaviorStatus) error {
	if strings.TrimSpace(step.Judge) == "" {
		return fmt.Errorf("step %d: judge required", step.Number)
	}
	if !falsifiable(step) {
		return fmt.Errorf("step %d: judge is not falsifiable: need a numeric bound, a state assertion, or a uses: reference", step.Number)
	}
	for _, key := range step.Uses {
		status, ok := behaviors.BehaviorStatus(key)
		if !ok {
			return fmt.Errorf("step %d: uses %q: no such corpus behavior", step.Number, key)
		}
		if status != "verified" {
			return fmt.Errorf("step %d: uses %q: behavior is %q, only verified behaviors may be used", step.Number, key, status)
		}
	}
	return nil
}

// stateAssertion matches the falsifiable-judge vocabulary: a judge is
// falsifiable when it pins a state ("stays disabled", "renders the badge").
// The list is deliberately boring — expansion is fine, vagueness is not.
var stateAssertion = regexp.MustCompile(`(?i)\b(?:renders?|shows?|displays?|opens?|contains?|matches?|equals?|remains?|stays?|visible|hidden|disabled|enabled|present|absent|exists?|returns?|appears?|loads?|redirects?|clears?|persists?|is|are)\b`)

// digitPattern detects a numeric bound inside a judge ("at least 12", "2.40").
var digitPattern = regexp.MustCompile(`\d`)

// falsifiable reports whether a judge carries at least one checkable
// predicate: a numeric bound, a state assertion, or a corpus reference.
// Agent-plausible but unverifiable judges ("the flow works nicely") reject.
func falsifiable(step Step) bool {
	if len(step.Uses) > 0 {
		return true
	}
	if digitPattern.MatchString(step.Judge) {
		return true
	}
	return stateAssertion.MatchString(step.Judge)
}

// tokenPattern extracts identifier-shaped tokens from a diff: camelCase,
// snake_case, or dotted/qualified identifiers. Generic prose words ("the",
// "request") do not match, so judges rarely false-positive.
var tokenPattern = regexp.MustCompile(`(?:[a-z]+[A-Z]|[a-z]+_[a-z]|(?:[A-Z]{2,}[A-Za-z0-9]*))[A-Za-z0-9_.]*`)

// DiffIdentifierTokens extracts the identifier tokens from a PR diff (the
// added/changed code the plan's judges were drafted against). Caller passes
// the diff text; dedup, sorted for deterministic output.
func DiffIdentifierTokens(diff string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, tok := range tokenPattern.FindAllString(diff, -1) {
		if !seen[tok] {
			seen[tok] = true
			tokens = append(tokens, tok)
		}
	}
	sort.Strings(tokens)
	return tokens
}

// LintFinding is one tautology-guard flag: a judge whose text names an
// identifier from the very diff it was drafted against — re-describing
// instead of asserting an outcome.
type LintFinding struct {
	Step  int    // step number
	Token string // the diff identifier named in the judge
	Judge string // full judge text for the approval UI
}

// LintDiffTokens flags judges that name identifiers extracted from the diff
// (DERV-04). Flagged, not rejected: a human judge may legitimately reference
// an implementation detail — the approval queue surfaces the flag so the
// reviewer sees the tautology risk. Empty diff tokens means nothing to check.
func LintDiffTokens(plan *Plan, tokens []string) []LintFinding {
	if len(tokens) == 0 {
		return nil
	}
	needle := map[string]bool{}
	for _, t := range tokens {
		needle[t] = true
	}
	var findings []LintFinding
	for _, step := range plan.Steps {
		words := wordBoundaryToken.FindAllString(step.Judge, -1)
		for _, w := range words {
			if needle[w] {
				findings = append(findings, LintFinding{Step: step.Number, Token: w, Judge: step.Judge})
				break
			}
		}
	}
	return findings
}

// wordBoundaryToken isolates identifier-shaped words inside a judge so a
// diff token only flags judges that actually name it (not a substring).
var wordBoundaryToken = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)
