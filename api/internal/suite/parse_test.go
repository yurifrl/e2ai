package suite

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/verdict"
)

func loadFixture(t *testing.T) *Report {
	t.Helper()
	data, err := os.ReadFile("testdata/results.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rep, err := Parse(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return rep
}

// TestParseFixture walks the real Playwright 1.58.2 report produced by
// fixturegen and pins the parser against its exact shape: test-level field
// named "status" (expected|unexpected|flaky|skipped), spec-carried identity,
// per-attempt results rows.
func TestParseFixture(t *testing.T) {
	rep := loadFixture(t)

	if rep.Version != "1.58.2" {
		t.Errorf("Version = %q, want 1.58.2", rep.Version)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("Errors = %v, want empty", rep.Errors)
	}
	if got, want := rep.Stats, (Stats{Duration: 2306.293, Expected: 1, Skipped: 1, Unexpected: 1, Flaky: 1}); got != want {
		t.Errorf("Stats = %+v, want %+v", got, want)
	}
	if len(rep.Tests) != 4 {
		t.Fatalf("len(Tests) = %d, want 4", len(rep.Tests))
	}

	byTitle := map[string]Test{}
	for _, te := range rep.Tests {
		if te.Suite != "fixture.spec.ts" {
			t.Errorf("%q: Suite = %q, want fixture.spec.ts", te.Title, te.Suite)
		}
		if te.File != "fixture.spec.ts" || te.Line == 0 || te.ID == "" {
			t.Errorf("%q: identity incomplete: file=%q line=%d id=%q", te.Title, te.File, te.Line, te.ID)
		}
		byTitle[te.Title] = te
	}

	passes := byTitle["passes"]
	if passes.Outcome != "expected" || passes.Status != "passed" || passes.Attempts != 1 || passes.ErrorMessage != "" {
		t.Errorf("passes = %+v", passes)
	}
	if passes.DurationMS != 1 {
		t.Errorf("passes.DurationMS = %d, want 1", passes.DurationMS)
	}

	fails := byTitle["always fails"]
	if fails.Outcome != "unexpected" || fails.Status != "failed" {
		t.Errorf("always fails outcome/status = %s/%s, want unexpected/failed", fails.Outcome, fails.Status)
	}
	if fails.Attempts != 2 {
		t.Errorf("always fails attempts = %d, want 2", fails.Attempts)
	}
	if !strings.Contains(fails.ErrorMessage, "expect") {
		t.Errorf("always fails.ErrorMessage = %q, want expect-related message", fails.ErrorMessage)
	}
	if strings.Contains(fails.ErrorMessage, "\x1b") {
		t.Errorf("always fails.ErrorMessage still carries ANSI escapes: %q", fails.ErrorMessage)
	}
	if fails.DurationMS <= 0 {
		t.Errorf("always fails.DurationMS = %d, want > 0", fails.DurationMS)
	}

	flaky := byTitle["flaky then passes"]
	if flaky.Outcome != "flaky" {
		t.Errorf("flaky outcome = %q, want flaky", flaky.Outcome)
	}
	if flaky.Attempts != 2 {
		t.Errorf("flaky attempts = %d, want 2", flaky.Attempts)
	}
	// The deterministic cause is the FIRST failed attempt, not the passing
	// retry's empty error.
	if flaky.Status != "passed" {
		t.Errorf("flaky.Status = %q, want passed (last attempt)", flaky.Status)
	}
	if flaky.ErrorMessage == "" {
		t.Errorf("flaky.ErrorMessage empty, want the failed attempt's cause")
	}

	skipped := byTitle["skipped"]
	if skipped.Outcome != "skipped" || skipped.Status != "skipped" {
		t.Errorf("skipped outcome/status = %s/%s, want skipped/skipped", skipped.Outcome, skipped.Status)
	}
}

// TestParseAttachmentsAndANSI exercises the shapes the fixture does not
// cover: attachments on attempts and ANSI-stripped top-level errors.
func TestParseAttachmentsAndANSI(t *testing.T) {
	report := `{
	  "config": {"configFile": "p.config.ts", "version": "1.58.2"},
	  "suites": [{
	    "title": "s.spec.ts", "file": "s.spec.ts", "line": 1, "column": 0,
	    "specs": [{
	      "title": "with evidence", "id": "spec-1", "file": "s.spec.ts", "line": 5, "column": 2,
	      "tests": [{
	        "projectName": "chromium",
	        "status": "unexpected",
	        "results": [{
	          "status": "failed", "duration": 42,
	          "error": {"message": "\u001b[31mboom\u001b[0m"},
	          "attachments": [
	            {"name": "screenshot", "contentType": "image/png", "path": "test-results/with-evidence/test-failed-1.png"},
	            {"name": "trace", "contentType": "application/zip", "path": "test-results/with-evidence/trace.zip"},
	            {"name": "inline", "contentType": "text/plain", "body": "aGVsbG8="}
	          ]
	        }]
	      }]
	    }]
	  }],
	  "errors": [{"message": "\u001b[1mglobal failure\u001b[0m"}],
	  "stats": {"duration": 42, "expected": 0, "skipped": 0, "unexpected": 1, "flaky": 0}
	}`

	rep, err := Parse([]byte(report))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rep.Tests) != 1 {
		t.Fatalf("len(Tests) = %d, want 1", len(rep.Tests))
	}
	te := rep.Tests[0]
	if te.Project != "chromium" {
		t.Errorf("Project = %q, want chromium", te.Project)
	}
	if got := te.ErrorMessage; got != "boom" {
		t.Errorf("ErrorMessage = %q, want boom", got)
	}
	if len(rep.Errors) != 1 || rep.Errors[0] != "global failure" {
		t.Errorf("Errors = %v, want [global failure]", rep.Errors)
	}

	want := []Attachment{
		{Name: "screenshot", ContentType: "image/png", Path: "test-results/with-evidence/test-failed-1.png"},
		{Name: "trace", ContentType: "application/zip", Path: "test-results/with-evidence/trace.zip"},
		// Body attachments (inline base64) carry no on-disk path.
		{Name: "inline", ContentType: "text/plain", Path: ""},
	}
	if len(te.Attachments) != len(want) {
		t.Fatalf("len(Attachments) = %d, want %d", len(te.Attachments), len(want))
	}
	for i, w := range want {
		if te.Attachments[i] != w {
			t.Errorf("Attachments[%d] = %+v, want %+v", i, te.Attachments[i], w)
		}
	}
}

// TestParseEmptyAndMalformed: zero-test reports parse fine; garbage is an
// error (the caller maps zero tests to INCONCLUSIVE via the verdict engine).
func TestParseEmptyAndMalformed(t *testing.T) {
	rep, err := Parse([]byte(`{"config": {"version": "1.58.2"}, "suites": [], "stats": {"duration": 0}}`))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(rep.Tests) != 0 {
		t.Errorf("len(Tests) = %d, want 0", len(rep.Tests))
	}

	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("Parse(malformed) = nil error, want error")
	}
}

// TestEvidenceMapping pins the suite→verdict bridge: attempts carry status +
// first-error, identity fields copy over. This is the row shape the verdict
// engine (VERD-01) consumes.
func TestEvidenceMapping(t *testing.T) {
	te := Test{
		ID: "spec-1", Title: "t", File: "f.spec.ts",
		attemptStatuses: []string{"failed", "passed"},
		attemptErrors:   []string{"cause", ""},
	}
	ev := te.Evidence()
	want := verdict.TestEvidence{
		ID:       "spec-1",
		Title:    "t",
		File:     "f.spec.ts",
		Status:   te.Outcome,
		Attempts: []verdict.Attempt{{Status: "failed", Error: "cause"}, {Status: "passed"}},
	}
	if ev.ID != want.ID || ev.Title != want.Title || ev.File != want.File || ev.Status != want.Status {
		t.Errorf("Evidence identity = %+v, want %+v", ev, want)
	}
	if len(ev.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2", len(ev.Attempts))
	}
	if ev.Attempts[0] != want.Attempts[0] || ev.Attempts[1] != want.Attempts[1] {
		t.Errorf("Attempts = %+v, want %+v", ev.Attempts, want.Attempts)
	}
}

// TestParseFixtureJSONValid guards testdata drift: the fixture must stay
// valid JSON (the parser's contract with the reporter).
func TestParseFixtureJSONValid(t *testing.T) {
	data, err := os.ReadFile("testdata/results.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
}
