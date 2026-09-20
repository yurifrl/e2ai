// Package verdict derives the fixed VERD-01 taxonomy (PASS / FAIL / FLAKY /
// INFRA_ERROR / INCONCLUSIVE) from parsed Playwright evidence.
//
// The package is pure: no I/O, no clocks, no exit codes. Verdicts derive
// exclusively from evidence rows per RESEARCH.md § Verdict Rules. A Playwright
// exit code alone is never sufficient (anti-pattern: "exit 0 with zero tests is
// not a pass").
//
// Evidence shapes are grounded in the real Playwright 1.58.2 output committed at
// api/internal/suite/testdata/results.json (T2 fixture):
//
//   - per-test top-level "status" ∈ expected | unexpected | flaky | skipped
//   - per-attempt "results[]" entries with "status" ∈ passed | failed |
//     timedOut | interrupted | skipped
package verdict

import "strconv"

// Verdict is the run- and test-level verdict taxonomy (VERD-01).
type Verdict string

const (
	VerdictPass         Verdict = "PASS"
	VerdictFail         Verdict = "FAIL"
	VerdictFlaky        Verdict = "FLAKY"
	VerdictInfraError   Verdict = "INFRA_ERROR"
	VerdictInconclusive Verdict = "INCONCLUSIVE"
)

// Test-level status values exactly as emitted by Playwright's JSON reporter
// (verified against the T2 fixture; note the field is named "status" at test
// level, and "results" per attempt).
const (
	StatusExpected   = "expected"
	StatusFlaky      = "flaky"
	StatusUnexpected = "unexpected"
	StatusSkipped    = "skipped"
)

// Attempt-level status values exactly as emitted by Playwright's JSON reporter.
const (
	AttemptPassed      = "passed"
	AttemptFailed      = "failed"
	AttemptTimedOut    = "timedOut"
	AttemptInterrupted = "interrupted"
	AttemptSkipped     = "skipped"
)

// Attempt is one execution of a test (a retry candidate) as parsed from a
// results[] row of the Playwright JSON report.
type Attempt struct {
	// Retry is the zero-based attempt index (0 = first run, 1 = first retry).
	Retry int
	// Status is the attempt outcome: passed | failed | timedOut | interrupted | skipped.
	Status string
	// Error is the first error message of the attempt (empty when it passed).
	// ANSI escapes from Playwright's messages are stripped by the parser.
	Error string
}

// TestEvidence is one parsed test row (one spec.test entry from the report).
type TestEvidence struct {
	// ID is the stable Playwright test id (spec id from the report) used as the
	// identity key for trend comparability (SUIT-02).
	ID string
	// Title is the spec title; File the spec file, both verbatim from the report.
	Title string
	File  string
	// Status is the test-level outcome: expected | unexpected | flaky | skipped.
	Status string
	// Attempts are the recorded per-attempt rows, in execution order.
	Attempts []Attempt
}

// RunInput is everything the verdict engine needs to derive a run verdict.
type RunInput struct {
	// Stage names the pipeline stage that produced (or failed to produce)
	// evidence — e.g. "wait-rollout", "execute". It is recorded in the
	// error_note when the runner never produced parseable results.
	Stage string
	// Results are the parsed test rows. Nil/empty means no parseable
	// results.json (crash, clone/build failure, rollout gate never healthy,
	// job timeout) — the run is INFRA_ERROR, never PASS/FAIL.
	Results []TestEvidence
}

// TestVerdict is the derived verdict for a single test.
type TestVerdict struct {
	ID      string
	Title   string
	File    string
	Verdict Verdict
	// Class is the failure classification (REPT-02) for FAIL/FLAKY tests;
	// empty for PASS and INCONCLUSIVE, and infra for INFRA_ERROR tests.
	Class Class
	// Reason is a short plain-language explanation grounded in the evidence.
	Reason string
}

// Counts is the run summary (persisted to runs.summary, SUIT-02).
type Counts struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Flaky        int `json:"flaky"`
	InfraError   int `json:"infra_error"`
	Inconclusive int `json:"inconclusive"`
}

// Total is the number of evaluated tests behind the counts.
func (c Counts) Total() int { return c.Pass + c.Fail + c.Flaky + c.InfraError + c.Inconclusive }

// Report is the derived run-level verdict with per-test detail.
type Report struct {
	Verdict Verdict
	// Note is the error_note: for INFRA_ERROR it names the responsible stage
	// (e.g. "execute: no parseable results.json"); for a clean verdict it is
	// empty.
	Note   string
	Counts Counts
	Tests  []TestVerdict
}

// Derive applies the run-level precedence (RESEARCH.md § Verdict Rules):
//
//  1. No parseable results (or any interrupted/missing-evidence test) → INFRA_ERROR
//     with the stage named in the note. Interrupted evidence means the runner
//     crashed mid-suite: the unrecorded remainder makes any PASS/FAIL unsound.
//  2. Any FAIL test → FAIL.
//  3. No FAIL, any FLAKY → FLAKY.
//  4. All evaluated tests PASS (at least one) → PASS.
//  5. No tests ran / all skipped → INCONCLUSIVE.
func Derive(in RunInput) Report {
	rep := Report{Verdict: VerdictInfraError, Note: infraNote(in.Stage, "no parseable results.json")}

	if len(in.Results) == 0 {
		return rep
	}

	rep.Tests = make([]TestVerdict, 0, len(in.Results))
	for _, t := range in.Results {
		tv := evaluate(t)
		rep.Tests = append(rep.Tests, tv)
		switch tv.Verdict {
		case VerdictPass:
			rep.Counts.Pass++
		case VerdictFail:
			rep.Counts.Fail++
		case VerdictFlaky:
			rep.Counts.Flaky++
		case VerdictInfraError:
			rep.Counts.InfraError++
		case VerdictInconclusive:
			rep.Counts.Inconclusive++
		}
	}

	switch {
	case rep.Counts.InfraError > 0:
		rep.Verdict = VerdictInfraError
		rep.Note = "execute: interrupted/missing evidence in " +
			intStr(rep.Counts.InfraError) + " test(s); results incomplete"
	case rep.Counts.Fail > 0:
		rep.Verdict = VerdictFail
	case rep.Counts.Flaky > 0:
		rep.Verdict = VerdictFlaky
	case rep.Counts.Pass > 0:
		// Rule 4: all evaluated tests PASS. Skipped tests are not evaluable and
		// do not block a PASS (they count as Inconclusive).
		rep.Verdict = VerdictPass
	default:
		// Rule 5: everything skipped → nothing was evaluated.
		rep.Verdict = VerdictInconclusive
		rep.Note = "no tests ran or all tests skipped; nothing to evaluate"
	}
	return rep
}

// evaluate maps one test's evidence to a verdict per the test-level table.
func evaluate(t TestEvidence) TestVerdict {
	tv := TestVerdict{ID: t.ID, Title: t.Title, File: t.File}

	switch t.Status {
	case StatusExpected:
		// Even an "expected" outcome is unsound without at least one recorded
		// attempt: Playwright always records attempts for executed tests.
		if _, ok := lastAttempt(t); !ok {
			tv.Verdict = VerdictInfraError
			tv.Reason = "expected outcome with no recorded attempt; evidence missing"
			return tv
		}
		tv.Verdict = VerdictPass
		return tv

	case StatusFlaky:
		// "failed some attempt, passed on retry" — a retry-recovered outcome
		// supersedes intermediate attempt statuses (including interrupts).
		tv.Verdict = VerdictFlaky
		tv.Class = Classify(t)
		tv.Reason = "failed on attempt " + intStr(firstFailedRetry(t)) + ", passed on retry"
		return tv

	case StatusSkipped:
		tv.Verdict = VerdictInconclusive
		tv.Reason = "test skipped; not evaluable"
		return tv

	case StatusUnexpected:
		last, ok := lastAttempt(t)
		if !ok {
			// Unexpected outcome with no recorded attempt: evidence is missing.
			tv.Verdict = VerdictInfraError
			tv.Reason = "unexpected outcome with no recorded attempt; evidence missing"
			return tv
		}
		switch last.Status {
		case AttemptFailed, AttemptTimedOut:
			tv.Verdict = VerdictFail
			tv.Class = Classify(t)
			tv.Reason = "final attempt " + last.Status + ": " + classifyReason(t)
			return tv
		case AttemptInterrupted:
			tv.Verdict = VerdictInfraError
			tv.Reason = "final attempt interrupted; runner crashed mid-test"
			return tv
		default:
			// Unknown or contradictory attempt status (e.g. passed/skipped under
			// "unexpected"): cannot invent an outcome from inconsistent evidence.
			tv.Verdict = VerdictInfraError
			tv.Reason = "unexpected outcome with final attempt status " + quote(last.Status) +
				"; inconsistent evidence"
			return tv
		}

	default:
		// Unknown test-level status: never invent — treat as missing evidence.
		tv.Verdict = VerdictInfraError
		tv.Reason = "unknown test status " + quote(t.Status) + "; evidence not parseable"
		return tv
	}
}

// lastAttempt returns the final recorded attempt, if any.
func lastAttempt(t TestEvidence) (Attempt, bool) {
	if len(t.Attempts) == 0 {
		return Attempt{}, false
	}
	return t.Attempts[len(t.Attempts)-1], true
}

// firstFailedRetry returns the retry index of the first failed/timed-out
// attempt, or -1 when none failed.
func firstFailedRetry(t TestEvidence) int {
	for _, a := range t.Attempts {
		if a.Status == AttemptFailed || a.Status == AttemptTimedOut {
			return a.Retry
		}
	}
	return -1
}

func infraNote(stage, detail string) string {
	if stage == "" {
		stage = "unknown-stage"
	}
	return stage + ": " + detail
}

func intStr(n int) string { return strconv.Itoa(n) }

func quote(s string) string { return `"` + s + `"` }
