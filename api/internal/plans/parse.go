// Package plans implements the declarative plan format (PLAN-01, sketch-006):
// a test plan is a markdown file — frontmatter for the scheduler, numbered
// intent steps for the agents. Parsing is pure: bytes in, typed Plan out,
// strict on every rule the format contract fixes (unknown frontmatter keys,
// enums, id shape, falsifiable judges). Database lookup rules (app exists,
// surfaces subset, uses resolve to verified behaviors) live in Validate and
// take narrow interfaces so tests never touch I/O.
package plans

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Plan is one parsed plan file. Content keeps the raw bytes as given; the
// content-hash identity (APPR-01) is computed over the normalized bytes.
type Plan struct {
	Frontmatter Frontmatter
	Steps       []Step
}

// Frontmatter mirrors the YAML block between the leading `---` fences.
// Unknown keys are rejected by the decoder — the format is closed, so a
// typo'd key fails at ingest instead of silently scheduling nothing.
type Frontmatter struct {
	ID       string `yaml:"id"`
	App      string `yaml:"app"`
	Title    string `yaml:"title"`
	Surfaces []string `yaml:"surfaces"`
	Trigger  string   `yaml:"trigger"`
	PRFilter *PRFilter `yaml:"pr-filter,omitempty"`
	Timeout  Duration  `yaml:"timeout,omitempty"`
	// Derived marks a machine-drafted plan (DERV-02). Derived plans must
	// carry Sources provenance IDs that are not all diff-derived (DERV-03).
	Derived bool     `yaml:"derived,omitempty"`
	Sources []string `yaml:"sources,omitempty"`
}

// PRFilter restricts an on-deploy plan to deploys whose PR touches matching
// paths (globs, gobwas/glob syntax — `**` works where path.Match does not).
type PRFilter struct {
	Paths []string `yaml:"paths"`
}

// Trigger enum: when the scheduler runs the plan.
const (
	TriggerOnDeploy = "on-deploy"
	TriggerOnDemand = "on-demand"
	TriggerNightly  = "nightly"
)

// OnFailure enum: what a surface does after a step fails. Stop is the
// zero value — the surface halts and reports; continue moves to the next
// step; diagnose runs a fresh session for analyst evidence.
const (
	OnFailureStop     = "stop"
	OnFailureContinue = "continue"
	OnFailureDiagnose = "diagnose"
)

// triggers is the closed set the frontmatter may carry.
var triggers = map[string]bool{TriggerOnDeploy: true, TriggerOnDemand: true, TriggerNightly: true}

// onFailures is the closed set a step may carry.
var onFailures = map[string]bool{OnFailureStop: true, OnFailureContinue: true, OnFailureDiagnose: true}

// idPattern pins plan identity: stable slug + semver suffix. Renaming the
// slug is a new plan; bumping the suffix is a new version.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-v\d+\.\d+\.\d+$`)

// keyPattern pins corpus behavior keys referenced by `uses:` (CORP-01 seeds
// keys like MATCH-LIVE-BADGE and PWSPEC-<TESTNAME>).
var keyPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]*$`)

// DefaultTimeout is the per-surface wall-clock budget when frontmatter omits
// one.
const DefaultTimeout = 10 * time.Minute

// Step is one numbered intent step from the body. Intent is the Navigator's
// driving instruction (plain language, no selectors); Judge is the Vision
// Judge's falsifiable acceptance check; Uses anchors the step to corpus
// behaviors; OnFailure defaults to stop.
type Step struct {
	Number    int
	Intent    string
	Judge     string
	Uses      []string
	OnFailure string
}

// Duration is a frontmatter timeout (`timeout: 10m`). Parsed eagerly so an
// unparseable budget fails at ingest, not at scheduling time.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	if dur <= 0 {
		return fmt.Errorf("timeout must be positive, got %s", dur)
	}
	*d = Duration(dur)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// fmFence matches the opening/closing frontmatter fence line.
var fmFence = regexp.MustCompile(`^---\s*$`)

// stepPrefix matches a numbered step opening line (`12. Navigate to home.`).
var stepPrefix = regexp.MustCompile(`^(\d+)\.\s*(.*)$`)

// fieldPrefix matches a step key line (`judge: ...` / `uses: ...`).
var fieldPrefix = regexp.MustCompile(`^\s*(judge|uses|on-failure):\s*(.*)$`)

// usesSeparator splits a comma-separated `uses:` value.
var usesSeparator = regexp.MustCompile(`\s*,\s*`)

// commentPrefix matches markdown comment lines, skipped during step parsing.
var commentPrefix = regexp.MustCompile(`^\s*#{1,6}\s`)

// Parse decodes one plan file. Errors carry the offending rule so ingest can
// surface actionable messages (invalid plans are never stored as versions).
func Parse(content []byte) (*Plan, error) {
	fmBody, body, err := splitFrontmatter(string(content))
	if err != nil {
		return nil, err
	}
	var fm Frontmatter
	dec := yaml.NewDecoder(strings.NewReader(fmBody), yaml.DisallowUnknownField())
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if err := validateFrontmatter(&fm); err != nil {
		return nil, err
	}
	steps, err := parseSteps(body)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("plan has no numbered steps")
	}
	if fm.Timeout == 0 {
		fm.Timeout = Duration(DefaultTimeout)
	}
	return &Plan{Frontmatter: fm, Steps: steps}, nil
}

// splitFrontmatter requires a leading `---` fence and returns the YAML
// document and the markdown body after the closing fence.
func splitFrontmatter(content string) (string, string, error) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || !fmFence.MatchString(lines[0]) {
		return "", "", fmt.Errorf("missing frontmatter: file must start with ---")
	}
	for i := 1; i < len(lines); i++ {
		if fmFence.MatchString(lines[i]) {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return "", "", fmt.Errorf("frontmatter never closed: missing second --- fence")
}

// validateFrontmatter checks every closed-set rule that needs no lookups:
// id shape, required refs, enums, and uses-key shape.
func validateFrontmatter(fm *Frontmatter) error {
	if fm.ID == "" {
		return fmt.Errorf("id: required")
	}
	if !idPattern.MatchString(fm.ID) {
		return fmt.Errorf("id %q: must match %s (slug + semver suffix, e.g. checkout-flow-v1.0.0)", fm.ID, idPattern)
	}
	if fm.App == "" {
		return fmt.Errorf("app: required")
	}
	if fm.Title == "" {
		return fmt.Errorf("title: required")
	}
	if len(fm.Surfaces) == 0 {
		return fmt.Errorf("surfaces: required, at least one")
	}
	if !triggers[fm.Trigger] {
		return fmt.Errorf("trigger %q: must be one of on-deploy|on-demand|nightly", fm.Trigger)
	}
	if fm.PRFilter != nil && len(fm.PRFilter.Paths) == 0 {
		return fmt.Errorf("pr-filter: present but paths is empty")
	}
	for _, s := range fm.Sources {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("sources: empty provenance id")
		}
	}
	return nil
}

// parseSteps walks the markdown body. A line matching `N. text` opens a
// step; indented or following plain lines extend the current field; `judge:`,
// `uses:`, and `on-failure:` lines set step fields (with continuation lines
// appending to judge/intent). Numbered gaps are allowed (steps may be
// authored sparsely); numbers must strictly increase.
func parseSteps(body string) ([]Step, error) {
	var steps []Step
	last := 0
	field := -1 // -1 none, 0 intent, 1 judge

	for i, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			field = -1 // blank line ends any continuation block
			continue
		}
		if commentPrefix.MatchString(line) {
			continue
		}
		if m := stepPrefix.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("line %d: step number %q: %w", i+1, m[1], err)
			}
			if n <= last {
				return nil, fmt.Errorf("line %d: step number %d must be greater than the previous step (%d)", i+1, n, last)
			}
			steps = append(steps, Step{Number: n, Intent: strings.TrimSpace(m[2]), OnFailure: OnFailureStop})
			last = n
			field = 0
			continue
		}
		if m := fieldPrefix.FindStringSubmatch(line); m != nil {
			if len(steps) == 0 {
				return nil, fmt.Errorf("line %d: %s: appears before the first numbered step", i+1, m[1])
			}
			step := &steps[len(steps)-1]
			value := strings.TrimSpace(m[2])
			switch m[1] {
			case "judge":
				step.Judge = value
				field = 1
			case "uses":
				keys, err := parseUses(value)
				if err != nil {
					return nil, fmt.Errorf("line %d: uses: %w", i+1, err)
				}
				step.Uses = keys
				field = -1
			case "on-failure":
				if !onFailures[value] {
					return nil, fmt.Errorf("line %d: on-failure %q: must be one of continue|diagnose (default stop)", i+1, value)
				}
				step.OnFailure = value
				field = -1
			}
			continue
		}
		// Continuation text: extends the step's intent or judge.
		if len(steps) == 0 || field == -1 {
			continue // prose before steps, or after a key field — not part of a step
		}
		if field == 0 {
			steps[len(steps)-1].Intent += " " + strings.TrimSpace(line)
		} else {
			steps[len(steps)-1].Judge += " " + strings.TrimSpace(line)
		}
	}
	return steps, nil
}

// parseUses splits a `uses:` value into corpus behavior keys. One key per
// comma-separated token; keys are pinned to the corpus key shape so a
// lowercase typo fails here with a clear message.
func parseUses(value string) ([]string, error) {
	if value == "" {
		return nil, fmt.Errorf("reference is empty")
	}
	var keys []string
	for _, token := range usesSeparator.Split(value, -1) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if !keyPattern.MatchString(token) {
			return nil, fmt.Errorf("behavior key %q: must match %s (e.g. MATCH-LIVE-BADGE)", token, keyPattern)
		}
		keys = append(keys, token)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("reference is empty")
	}
	return keys, nil
}

// ContentHash is the version identity (APPR-01): sha256 over the normalized
// bytes. Normalization is CRLF→LF plus trailing-whitespace stripping on each
// line (and trailing blank lines at EOF) — editors and OSes must not change a
// version's identity; any other byte change does.
func ContentHash(content []byte) string {
	sum := sha256.Sum256(normalize(content))
	return hex.EncodeToString(sum[:])
}

// normalize applies the content-hash normalization rules, keeping the file
// verbatim otherwise.
func normalize(content []byte) []byte {
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return []byte(strings.Join(lines, "\n"))
}
