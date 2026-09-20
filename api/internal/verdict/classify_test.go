package verdict

import "testing"

// Classification rows use error messages in the real shapes Playwright emits
// (T2 fixture carries the assertion form: "expect(1).toBe(2) // Object.is
// equality" for the always-fails spec).

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		in   TestEvidence
		want Class
	}{
		{
			// Fixture "always fails": identical assertion error on attempt 0 and
			// retry 1 — deterministic, wrong-but-stable expectation.
			name: "identical assertion failures across attempts are test_gap",
			in: mk("bd68f91cbcaac3bd5681", "always fails", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"},
				Attempt{Retry: 1, Status: AttemptFailed, Error: "expect(1).toBe(2) // Object.is equality"}),
			want: ClassTestGap,
		},
		{
			name: "single-attempt assertion failure is test_gap",
			in: mk("s1", "fails once", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "Expected: \"beta\"\nReceived: \"alpha\""}),
			want: ClassTestGap,
		},
		{
			name: "MSW unhandled request is test_gap (fixtures under-mock)",
			in: mk("s2", "missing mock", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "[MSW] Cannot find handler for POST /api/auth"}),
			want: ClassTestGap,
		},
		{
			name: "clone failure is infra",
			in: mk("s3", "clone broke", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "fatal: repository 'bet-client' not found"}),
			want: ClassInfra,
		},
		{
			name: "webServer/connection refusal is infra",
			in: mk("s4", "serve never came up", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "webServer process failed; ECONNREFUSED localhost:3000"}),
			want: ClassInfra,
		},
		{
			// Reproduces across attempts, non-assertion language → deterministic
			// UI failure: app_bug.
			name: "reproducing TypeError is app_bug",
			in: mk("s5", "render crash", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "TypeError: Cannot read properties of undefined (reading 'map')"},
				Attempt{Retry: 1, Status: AttemptFailed, Error: "TypeError: Cannot read properties of undefined (reading 'map')"}),
			want: ClassAppBug,
		},
		{
			// infra always wins, even with crash language present.
			name: "infra outranks app_bug language",
			in: mk("s6", "both signals", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "webServer timeout before page.evaluate"},
				Attempt{Retry: 1, Status: AttemptFailed, Error: "webServer timeout before page.evaluate"}),
			want: ClassInfra,
		},
		{
			// Deterministic reproducibility with no recognizable pattern falls to
			// app_bug; unclassifiable single-shot falls to test_gap.
			name: "unclassifiable single-attempt failure defaults to test_gap",
			in: mk("s7", "odd failure", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "the moon was full"}),
			want: ClassTestGap,
		},
		{
			name: "unclassifiable reproducible failure defaults to app_bug",
			in: mk("s8", "odd reproducible", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "the moon was full"},
				Attempt{Retry: 1, Status: AttemptFailed, Error: "the moon was full"}),
			want: ClassAppBug,
		},
		{
			// A pass on retry means the failure did NOT reproduce → not app_bug.
			name: "recovery on retry suppresses app_bug",
			in: mk("s9", "crashed then fine", "fixture.spec.ts", StatusFlaky,
				Attempt{Retry: 0, Status: AttemptFailed, Error: "TypeError: Cannot read properties of undefined"},
				Attempt{Retry: 1, Status: AttemptPassed}),
			want: ClassTestGap,
		},
		{
			// empty error message matches nothing → default branch.
			name: "empty error message defaults to test_gap",
			in: mk("s10", "no message", "fixture.spec.ts", StatusUnexpected,
				Attempt{Retry: 0, Status: AttemptFailed}),
			want: ClassTestGap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.in)
			if got != tt.want {
				t.Errorf("class = %q, want %q", got, tt.want)
			}
			// Reserved rule: agent_flake cannot fire in P1 (no agents exist).
			if got == ClassAgentFlake {
				t.Error("agent_flake must never be returned in P1")
			}
		})
	}
}
