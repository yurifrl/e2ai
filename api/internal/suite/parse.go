// Package suite runs the deterministic Playwright suite for one run and
// parses its JSON report. The parser is pure: results.json in, typed rows
// out — no I/O, no exit codes (VERD-01). The execute River job (worked by the
// runner container) and the fixture generator both live here.
//
// The parse shapes are grounded in the real Playwright 1.58.2 report at
// testdata/results.json (fixturegen): the test-level verdict field is named
// "status" with values expected | unexpected | flaky | skipped, per-spec
// identity lives on the spec entry ("id"), and per-attempt rows live in
// results[] with status passed | failed | skipped | timedOut | interrupted.
package suite

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/verdict"
)

// parseReport is the raw JSON-report shape (subset we consume).
type parseReport struct {
	Config struct {
		ConfigFile string `json:"configFile"`
		Version    string `json:"version"`
	} `json:"config"`
	Suites []parseSuite `json:"suites"`
	Errors []parseError `json:"errors"`
	Stats  parseStats   `json:"stats"`
}

type parseSuite struct {
	Title  string       `json:"title"`
	Suites []parseSuite `json:"suites"`
	Specs  []parseSpec  `json:"specs"`
}

type parseSpec struct {
	Title  string      `json:"title"`
	ID     string      `json:"id"`
	File   string      `json:"file"`
	Line   int         `json:"line"`
	Column int         `json:"column"`
	Tests  []parseTest `json:"tests"`
}

type parseTest struct {
	ProjectName string            `json:"projectName"`
	Status      string            `json:"status"` // test-level outcome enum
	Results     []parseAttempt    `json:"results"`
	Annotations []json.RawMessage `json:"annotations"`
}

type parseAttempt struct {
	Status      string     `json:"status"` // passed | failed | skipped | timedOut | interrupted
	Duration    float64    `json:"duration"`
	Attachments []parseAtt `json:"attachments"`
	// Playwright 1.58 writes a single error object per attempt; older shapes
	// carry an errors[] array. Both are accepted.
	Error  parseAttErr   `json:"error"`
	Errors []parseAttErr `json:"errors"`
}

type parseAtt struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Path        string `json:"path"`
}

type parseAttErr struct {
	Message string `json:"message"`
}

type parseError struct {
	Message string `json:"message"`
}

type parseStats struct {
	Duration   float64 `json:"duration"`
	Expected   int     `json:"expected"`
	Skipped    int     `json:"skipped"`
	Unexpected int     `json:"unexpected"`
	Flaky      int     `json:"flaky"`
}

// Report is the parsed run-level report.
type Report struct {
	// ConfigFile and Version come from the report's config block; Version is
	// the Playwright version that produced it (drift canary vs the runner
	// image pin).
	ConfigFile string
	Version    string
	// Errors carries the reporter's top-level errors (suite-level crashes,
	// config problems) verbatim, ANSI-stripped.
	Errors []string
	Stats  Stats
	Tests  []Test
}

// Stats mirrors the report's stats block. Duration is milliseconds.
type Stats struct {
	Duration   float64
	Expected   int
	Skipped    int
	Unexpected int
	Flaky      int
}

// Attachment is one evidence file pointer from an attempt row: the name
// Playwright gave it (e.g. "screenshot", "trace"), its content type and the
// on-disk path as recorded by the reporter (relative to the runner's
// test-results directory).
type Attachment struct {
	Name        string
	ContentType string
	Path        string
}

// Test is one parsed test row (one spec.test entry from the report).
type Test struct {
	// ID is the spec id from the report — the stable identity key for trend
	// comparability (SUIT-02). Older reporter shapes lack it; the parser
	// falls back to "file:line".
	ID string
	// Suite is the enclosing suite-title chain joined with " › " (empty for
	// top-level tests).
	Suite string
	Title string
	File  string
	Line  int
	// Project is the Playwright project name (empty for single-project runs).
	Project string
	// Outcome is the test-level verdict enum from the report ("status" field):
	// expected | unexpected | flaky | skipped.
	Outcome string
	// Status is the last attempt's status (passed | failed | skipped |
	// timedOut | interrupted).
	Status string
	// Attempts is the number of recorded attempts (retries included).
	Attempts int
	// DurationMS sums attempt durations (retries included).
	DurationMS int
	// ErrorMessage is the first non-empty attempt error, earliest attempt
	// first — the deterministic cause, not the final attempt's noise.
	// ANSI escapes are stripped.
	ErrorMessage string
	Attachments  []Attachment

	// Per-attempt parallel slices kept from the parse walk so Evidence can
	// rebuild verdict rows without re-parsing. Same length as Attempts.
	attemptStatuses []string
	attemptErrors   []string
}

// Evidence converts the parsed row into the verdict engine's evidence shape.
// The classification column and run verdict both derive from this — parsed
// report in, verdict out, no exit codes anywhere.
func (t Test) Evidence() verdict.TestEvidence {
	att := make([]verdict.Attempt, 0, len(t.attemptErrors))
	for i, msg := range t.attemptErrors {
		att = append(att, verdict.Attempt{Status: t.attemptStatuses[i], Error: msg})
	}
	return verdict.TestEvidence{
		ID:       t.ID,
		Title:    t.Title,
		File:     t.File,
		Status:   t.Outcome,
		Attempts: att,
	}
}

// Parse decodes a Playwright JSON report. Malformed JSON is an error; a
// well-formed report with zero tests parses fine (the caller decides what
// zero tests means — the verdict engine maps it to INCONCLUSIVE/INFRA_ERROR
// per VERD-01).
func Parse(data []byte) (*Report, error) {
	var raw parseReport
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse playwright report: %w", err)
	}
	rep := &Report{
		ConfigFile: raw.Config.ConfigFile,
		Version:    raw.Config.Version,
		Stats: Stats{
			Duration:   raw.Stats.Duration,
			Expected:   raw.Stats.Expected,
			Skipped:    raw.Stats.Skipped,
			Unexpected: raw.Stats.Unexpected,
			Flaky:      raw.Stats.Flaky,
		},
	}
	for _, e := range raw.Errors {
		rep.Errors = append(rep.Errors, stripANSI(e.Message))
	}
	for _, s := range raw.Suites {
		walkSuite(s, nil, rep)
	}
	return rep, nil
}

// walkSuite recurses the suite tree, carrying the title chain.
func walkSuite(s parseSuite, chain []string, rep *Report) {
	titles := chain
	if s.Title != "" {
		titles = append(chain, s.Title)
	}
	for _, sub := range s.Suites {
		walkSuite(sub, titles, rep)
	}
	for _, spec := range s.Specs {
		for _, t := range spec.Tests {
			rep.Tests = append(rep.Tests, buildTest(spec, t, titles))
		}
	}
}

// buildTest maps one spec.test entry onto a Test row. One spec per project;
// multi-project reports emit one row per project (distinct spec ids).
func buildTest(spec parseSpec, t parseTest, titles []string) Test {
	id := spec.ID
	if id == "" {
		id = spec.File + ":" + strconv.Itoa(spec.Line)
	}

	statuses := make([]string, 0, len(t.Results))
	errs := make([]string, 0, len(t.Results))
	firstErr := ""
	for _, a := range t.Results {
		statuses = append(statuses, a.Status)
		msg := attemptError(a)
		errs = append(errs, msg)
		if firstErr == "" && msg != "" {
			firstErr = msg
		}
	}

	dur := 0
	atts := make([]Attachment, 0, len(t.Results))
	for _, a := range t.Results {
		dur += int(a.Duration)
		for _, at := range a.Attachments {
			atts = append(atts, Attachment{Name: at.Name, ContentType: at.ContentType, Path: at.Path})
		}
	}

	return Test{
		ID:              id,
		Suite:           strings.Join(titles, " › "),
		Title:           spec.Title,
		File:            spec.File,
		Line:            spec.Line,
		Project:         t.ProjectName,
		Outcome:         t.Status,
		Status:          lastStatus(statuses),
		Attempts:        len(t.Results),
		DurationMS:      dur,
		ErrorMessage:    firstErr,
		Attachments:     atts,
		attemptStatuses: statuses,
		attemptErrors:   errs,
	}
}

// attemptError returns the attempt's first error message, ANSI-stripped.
// Accepts the singular error object (Playwright 1.58) and the errors[] array
// (older reporter shapes); "" when the attempt carries none.
func attemptError(a parseAttempt) string {
	if a.Error.Message != "" {
		return stripANSI(a.Error.Message)
	}
	if len(a.Errors) > 0 {
		return stripANSI(a.Errors[0].Message)
	}
	return ""
}

// lastStatus returns the final attempt's status ("" for zero attempts).
func lastStatus(statuses []string) string {
	if len(statuses) == 0 {
		return ""
	}
	return statuses[len(statuses)-1]
}

// ansiStrip matches ANSI escape sequences (CSI and OSC variants) Playwright
// embeds in error messages.
var ansiStrip = regexp.MustCompile(`\x1b(?:\[[0-9;:?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

// stripANSI removes ANSI escapes; nil-safe for empty messages.
func stripANSI(s string) string {
	if s == "" {
		return ""
	}
	return ansiStrip.ReplaceAllString(s, "")
}
