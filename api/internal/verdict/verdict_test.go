package verdict

import (
	"strings"
	"testing"
)

// All table rows cite real values from the T2 fixture
// (api/internal/suite/testdata/results.json — Playwright 1.58.2 JSON report).
// Test-level statuses observed there: expected, unexpected, flaky, skipped.
// Attempt-level statuses observed there: passed, failed, skipped (timedOut and
// interrupted are enum members of the same reporter field, exercised synthetically).

func mk(id, title, file, status string, attempts ...Attempt) TestEvidence {
	return TestEvidence{ID: id, Title: title, File: file, Status: status, Attempts: attempts}
}

func TestEvaluatePerTest(t *testing.T) {
	tests := []struct {
		name    string
		in      TestEvidence
		want    Verdict
		wantCls Class
	}{
		{
			// fixture spec "passes": status=expected, one attempt passed.
			name: "expected outcome is PASS",
			in: mk("f6ca7a37941598886397", "passes", "fixture.spec.ts", StatusExpected,
				Attempt{Retry: 0, Status: AttemptPassed}),
			want: VerdictPass,
		},
		{
			// fixture spec "always fails": status=unexpected, both attempts failed.
			name: "unexpected with failed final attempt is FAIL (fixture spec)",
			in: mk("bd68f91cbcaac3bd5681", "always fails", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"},
				Attempt{Retry: 1, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"}),
			want:    VerdictFail,
			wantCls: ClassTestGap,
		},
		{
			name: "unexpected with timedOut final attempt is FAIL",
			in: mk("synthetic-timeout", "times out", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed},
				Attempt{Retry: 1, Status: AttemptTimedOut}),
			want:    VerdictFail,
			wantCls: ClassAppBug,
		},
		{
			// fixture spec "flaky then passes": status=flaky, attempt 0 failed,
			// attempt 1 passed on retry.
			name: "flaky outcome is FLAKY (fixture spec)",
			in: mk("af05497c2c690d96c44c", "flaky then passes", "fixture.spec.ts", StatusFlaky,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"},
				Attempt{Retry: 1, Status: AttemptPassed}),
			want:    VerdictFlaky,
			wantCls: ClassTestGap,
		},
		{
			// fixture spec "skipped": status=skipped with fixme annotation,
			// attempt status=skipped.
			name: "skipped outcome is INCONCLUSIVE (fixture spec)",
			in: mk("0cba56783d82909a19bf", "skipped", "fixture.spec.ts", StatusSkipped,
				Attempt{Retry: 0, Status: AttemptSkipped}),
			want: VerdictInconclusive,
		},
		{
			name: "skipped outcome with no recorded attempt is still INCONCLUSIVE",
			in:   mk("synthetic-skip", "skipped no attempt", "fixture.spec.ts", StatusSkipped),
			want: VerdictInconclusive,
		},
		{
			// interrupted is an enum member of the attempt field (Playwright 1.58.2).
			name: "unexpected with interrupted final attempt is INFRA_ERROR",
			in: mk("synthetic-interrupt", "crashed mid-test", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed},
				Attempt{Retry: 1, Status: AttemptInterrupted}),
			want: VerdictInfraError,
		},
		{
			name: "unexpected outcome with no recorded attempt is INFRA_ERROR (missing evidence)",
			in:   mk("synthetic-missing", "no attempts recorded", "fixture.spec.ts", StatusUnexpected),
			want: VerdictInfraError,
		},
		{
			name: "expected outcome with no recorded attempt is INFRA_ERROR (missing evidence)",
			in:   mk("synthetic-missing-pass", "no attempts but expected", "fixture.spec.ts", StatusExpected),
			want: VerdictInfraError,
		},
		{
			name: "unknown test-level status is INFRA_ERROR (never invent an outcome)",
			in: mk("synthetic-unknown", "weird status", "fixture.spec.ts", "cancelled",
				Attempt{Retry: 0, Status: AttemptPassed}),
			want: VerdictInfraError,
		},
		{
			name: "unexpected with contradictory passed final attempt is INFRA_ERROR",
			in: mk("synthetic-contradiction", "passed yet unexpected", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptPassed}),
			want: VerdictInfraError,
		},
		{
			// flaky outcome (retry recovered) supersedes a prior interrupted
			// attempt: the run completed the test deterministically.
			name: "interrupted attempt within flaky outcome is superseded by final FLAKY",
			in: mk("synthetic-flaky-after-interrupt", "interrupted then passed", "fixture.spec.ts", StatusFlaky,
				Attempt{Retry: 0, Status: AttemptInterrupted},
				Attempt{Retry: 1, Status: AttemptPassed}),
			want:    VerdictFlaky,
			wantCls: ClassTestGap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluate(tt.in)
			if got.Verdict != tt.want {
				t.Errorf("verdict = %q, want %q (reason: %s)", got.Verdict, tt.want, got.Reason)
			}
			if got.Class != tt.wantCls {
				t.Errorf("class = %q, want %q", got.Class, tt.wantCls)
			}
		})
	}
}

func TestDeriveRunPrecedence(t *testing.T) {
	pass := mk("t1", "passes", "fixture.spec.ts", StatusExpected, Attempt{Status: AttemptPassed})
	fail := mk("t2", "always fails", "fixture.spec.ts", StatusUnexpected,
		Attempt{Status: AttemptFailed}, Attempt{Status: AttemptFailed})
	flaky := mk("t3", "flaky then passes", "fixture.spec.ts", StatusFlaky,
		Attempt{Status: AttemptFailed}, Attempt{Status: AttemptPassed})
	skipped := mk("t4", "skipped", "fixture.spec.ts", StatusSkipped, Attempt{Status: AttemptSkipped})
	interrupted := mk("t5", "crashed", "fixture.spec.ts", StatusUnexpected,
		Attempt{Status: AttemptFailed}, Attempt{Status: AttemptInterrupted})
	infraFlake := mk("t6", "boots the emulator", "fixture.spec.ts", StatusUnexpected,
		Attempt{Status: AttemptFailed})
	infraFlake.FailureClass = string(FailureClassInfraFlakeBoot)
	infraFlake.FailureDetail = "bootstatus poll exceeded 90s: emulator never left the boot animation"

	tests := []struct {
		name    string
		in      RunInput
		want    Verdict
		wantNt  string
		pas, fa int
		fl, ie  int
		ifk     int
		ic      int
	}{
		{
			// Rule 1: runner never produced parseable results.json.
			name:   "no parseable results is INFRA_ERROR naming the stage",
			in:     RunInput{Stage: "wait-rollout"},
			want:   VerdictInfraError,
			wantNt: "wait-rollout: no parseable results.json",
		},
		{
			// Rule 1 (variant): interrupted test mid-suite = incomplete evidence.
			name: "any interrupted test outranks a FAIL",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{fail, interrupted}},
			want: VerdictInfraError,
			pas:  0, fa: 1, fl: 0, ie: 1, ic: 0,
		},
		{
			// Rule 2: any FAIL → FAIL.
			name: "any FAIL outranks FLAKY and skipped",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{flaky, fail, skipped, pass}},
			want: VerdictFail,
			pas:  1, fa: 1, fl: 1, ie: 0, ic: 1,
		},
		{
			// Rule 3: no FAIL, any FLAKY → FLAKY.
			name: "FLAKY outranks PASS",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{flaky, pass, skipped}},
			want: VerdictFlaky,
			pas:  1, fa: 0, fl: 1, ie: 0, ic: 1,
		},
		{
			// Rule 4: all evaluated tests PASS → PASS (skipped do not block it).
			name: "all PASS with one skipped is PASS",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{pass, skipped}},
			want: VerdictPass,
			pas:  1, fa: 0, fl: 0, ie: 0, ic: 1,
		},
		{
			// PLAT-04 Rule 3: no FAIL, any INFRA_FLAKE → INFRA_FLAKE; the
			// device environment was unstable, no app-behavior signal.
			name: "INFRA_FLAKE outranks FLAKY and PASS",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{infraFlake, flaky, pass}},
			want: VerdictInfraFlake,
			pas:  1, fa: 0, fl: 1, ie: 0, ifk: 1, ic: 0,
		},
		{
			// Rule 2 still outranks the device layer: a real FAIL is a
			// stronger signal than an unstable device alongside it.
			name: "FAIL outranks INFRA_FLAKE",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{infraFlake, fail, pass}},
			want: VerdictFail,
			pas:  1, fa: 1, fl: 0, ie: 0, ifk: 1, ic: 0,
		},
		{
			// Rule 5: no tests ran / all skipped → INCONCLUSIVE.
			name: "all skipped is INCONCLUSIVE",
			in:   RunInput{Stage: "execute", Results: []TestEvidence{skipped}},
			want: VerdictInconclusive,
			pas:  0, fa: 0, fl: 0, ie: 0, ic: 1,
		},
		{
			name:   "empty results slice with stage is INFRA_ERROR",
			in:     RunInput{Stage: "execute", Results: []TestEvidence{}},
			want:   VerdictInfraError,
			wantNt: "execute: no parseable results.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Derive(tt.in)
			if got.Verdict != tt.want {
				t.Errorf("verdict = %q, want %q", got.Verdict, tt.want)
			}
			if tt.wantNt != "" && got.Note != tt.wantNt {
				t.Errorf("note = %q, want %q", got.Note, tt.wantNt)
			}
			if got.Counts.Pass != tt.pas || got.Counts.Fail != tt.fa ||
				got.Counts.Flaky != tt.fl || got.Counts.InfraError != tt.ie ||
				got.Counts.InfraFlake != tt.ifk ||
				got.Counts.Inconclusive != tt.ic {
				t.Errorf("counts = %+v, want pass=%d fail=%d flaky=%d infra=%d infra_flake=%d inconclusive=%d",
					got.Counts, tt.pas, tt.fa, tt.fl, tt.ie, tt.ifk, tt.ic)
			}
			if len(got.Tests) != len(tt.in.Results) {
				t.Errorf("got %d test verdicts, want %d", len(got.Tests), len(tt.in.Results))
			}
		})
	}
}

// TestInfraFlakeOverride (PLAT-04): the driver-typed failure class wins over
// the deterministic heuristics only when it is an infra_flake value (exact or
// subtype); anything else keeps APP_BUG semantics.
func TestInfraFlakeOverride(t *testing.T) {
	tests := []struct {
		name       string
		failClass  string
		detail     string
		want       Verdict
		wantCls    Class
		wantReason string // substring the reason must carry
	}{
		{
			name:       "unqualified infra_flake class overrides the heuristics",
			failClass:  string(FailureClassInfraFlake),
			want:       VerdictInfraFlake,
			wantCls:    ClassInfraFlake,
			wantReason: "device environment unstable before the app could be exercised",
		},
		{
			name:       "subtype-qualified class carries the subtype in the reason",
			failClass:  string(FailureClassInfraFlakeBoot),
			detail:     "emulator never left the boot animation",
			want:       VerdictInfraFlake,
			wantCls:    ClassInfraFlake,
			wantReason: "probe: boot_timeout): emulator never left the boot animation",
		},
		{
			// APP_BUG semantics: a driver class value outside the infra_flake
			// vocabulary is ignored, the deterministic heuristics decide
			// (an assertion failure here → test_gap).
			name:      "non-infra_flake class value is ignored (APP_BUG semantics)",
			failClass: "app_bug",
			want:      VerdictFail,
			wantCls:   ClassTestGap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := mk("t1", "never got a fair device", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"})
			in.FailureClass = tt.failClass
			in.FailureDetail = tt.detail

			got := evaluate(in)
			if got.Verdict != tt.want {
				t.Errorf("verdict = %q, want %q (reason: %s)", got.Verdict, tt.want, got.Reason)
			}
			if got.Class != tt.wantCls {
				t.Errorf("class = %q, want %q", got.Class, tt.wantCls)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}
