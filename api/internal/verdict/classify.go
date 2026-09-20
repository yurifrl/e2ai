package verdict

import "strings"

// Failure classification (REPT-02, Phase 1 heuristics). Phase 3 replaces this
// with agent reasoning; until then these are deterministic error-pattern rules
// over the evidence already parsed from results.json.
type Class string

const (
	// ClassInfra: runner/clone/build/serve failures, job timeout, rollout gate
	// timeout. The suite never got a fair environment.
	ClassInfra Class = "infra"
	// ClassTestGap: assertion failure with a clean MSW interaction — the
	// expectation reads as wrong-but-stable: a deterministic, reproducible
	// failure on mocked data ⇒ spec vs fixture disagreement. Includes MSW
	// unhandled-request failures (the fixtures under-mock an endpoint the spec
	// exercises — a suite gap, not an app defect).
	ClassTestGap Class = "test_gap"
	// ClassAppBug: deterministic UI failure against the mocked fixtures that
	// reproduces across attempts — crash, broken render, state corruption.
	ClassAppBug Class = "app_bug"
	// ClassAgentFlake: reserved for the Phase 3 agent layer (agent-driven
	// exploration flakes). The rule exists for taxonomy completeness only — it
	// CANNOT fire in P1 because P1 runs no agents; no heuristic below returns
	// it, and classify() never produces it.
	ClassAgentFlake Class = "agent_flake"
	// ClassInfraFlake: the mobile/device layer failed before the app under
	// test could be exercised (PLAT-04). Driver readiness probes classify
	// boot/screenshot failures as infra_flake — the device environment was
	// unstable, so the failure carries no signal about app behavior. Unlike
	// ClassInfra (the whole run never got a fair environment), an infra_flake
	// test can coexist with passing tests on the same run.
	ClassInfraFlake Class = "infra_flake"
)

// Driver-typed failure-class vocabulary (PLAT-04, the locked INFRA_FLAKE
// contract): TS drivers emit "infra_flake" — optionally subtype-qualified as
// "infra_flake:<subtype>" — into the failure-class field of a step/test
// result. The Go verdict engine reads it back verbatim; anything that is not
// an infra_flake value keeps APP_BUG semantics (the deterministic heuristics
// below decide).
//
// Known subtypes (drivers may add more; unknown subtypes still classify as
// infra_flake):
//
//	boot_timeout      — device/emulator/simulator boot or bootstatus poll timed out
//	device_offline    — the device dropped off adb/idb/simctl mid-session
//	screenshot_failed — screenshot capture failed or produced an unusable frame
//	probe_timeout     — a readiness probe (animation/network/render) never settled
const (
	FailureClassInfraFlake          = ClassInfraFlake // canonical, unqualified
	FailureClassInfraFlakeBoot      = "infra_flake:boot_timeout"
	FailureClassInfraFlakeOffline   = "infra_flake:device_offline"
	FailureClassInfraFlakeShots     = "infra_flake:screenshot_failed"
	FailureClassInfraFlakeProbe     = "infra_flake:probe_timeout"
	FailureClassInfraFlakeStability = "infra_flake:unstable_frames"
)

// IsInfraFlake reports whether a driver-typed failure class is an
// infra_flake value (the exact class or any subtype). The vocabulary is
// lowercase-snake per the locked contract; the match is case-sensitive.
func IsInfraFlake(fc string) bool {
	return fc == string(FailureClassInfraFlake) || strings.HasPrefix(fc, string(FailureClassInfraFlake)+":")
}

// Deterministic error-pattern rules. Keywords are lowercase; messages are
// lowercased before matching (Playwright messages carry ANSI noise that the
// parser strips).
var infraPatterns = []string{
	// clone / checkout
	"git clone", "git checkout", "fatal: repository", "authentication failed",
	// build / install
	"yarn install", "npm install", "corepack", "lockfile", "build failed",
	"command failed", "module not found", "cannot find module",
	// serve / webServer / network to the app under test
	"webserver", "web server", "econnrefused", "connection refused",
	"etimedout", "econnreset", "net::err", "localhost:3000",
	// runner / gate timeouts
	"timed out", "timeout", "rollout gate", "health_url", "playwright finished",
	"browser closed unexpectedly", "browser crashed",
}

var appBugPatterns = []string{
	// crash / broken render / state corruption — language-level crashes and
	// DOM-level breakage, not expectation disagreement.
	"typeerror", "referenceerror", "syntaxerror", "page.evaluate",
	"page crash", "page.goto: net::", "navigation failed", "hydration",
	"undefined is not", "cannot read propert", "corrupted",
}

var assertionPatterns = []string{
	"expect(", "expected:", "received:", "tobe", "tohave", "tocontain",
	"tomatchsnapshot", "locator expect", "soft expect",
}

var mswMismatchPatterns = []string{
	// playwright-msw / msw unhandled-request signatures.
	"cannot find handler", "no handler", "unhandled request", "msw",
}

// maxFlakeReason caps the boot-log citation embedded in a test reason so the
// report/comment surfaces stay readable; the full log lives in the step/
// test-result attachments.
const maxFlakeReason = 400

// infraFlakeReason renders the plain-language reason for an infra_flake
// test: subtype + the driver's boot-log citation (truncated).
func infraFlakeReason(t TestEvidence) string {
	sub := strings.TrimPrefix(t.FailureClass, string(FailureClassInfraFlake))
	sub = strings.TrimPrefix(sub, ":")
	detail := t.FailureDetail
	if len(detail) > maxFlakeReason {
		detail = detail[:maxFlakeReason] + "…"
	}
	if sub == "" && detail == "" {
		return "device environment unstable before the app could be exercised"
	}
	if sub == "" {
		return "device environment unstable before the app could be exercised: " + detail
	}
	if detail == "" {
		return "device environment unstable (probe: " + sub + ")"
	}
	return "device environment unstable (probe: " + sub + "): " + detail
}

// Classify assigns a REPT-02 class to a failing (FAIL or FLAKY) test and
// returns a plain-language reason. Precedence:
//
//  1. infra patterns (environment never worked) — always wins;
//  2. crash/render language reproducing across attempts — app_bug;
//  3. assertion failure or MSW mock mismatch — test_gap;
//  4. anything else deterministic across attempts — app_bug;
//  5. default — test_gap (wrong-but-stable expectation is the safest
//     report-only reading of an unclassifiable deterministic failure).
//
// ClassAgentFlake is intentionally never returned (see its doc comment).
func Classify(t TestEvidence) Class {
	reproducible := reproducibleAcrossAttempts(t)
	class, _ := classifyMessage(firstError(t), reproducible)
	return class
}

// classifyReason mirrors Classify but returns the human-readable reason text.
func classifyReason(t TestEvidence) string {
	reproducible := reproducibleAcrossAttempts(t)
	_, reason := classifyMessage(firstError(t), reproducible)
	return reason
}

func classifyMessage(errMsg string, reproducible bool) (Class, string) {
	msg := lower(errMsg)
	if matchAny(msg, infraPatterns) {
		return ClassInfra, "environment/runner failure detected in error output"
	}
	if matchAny(msg, appBugPatterns) && reproducible {
		return ClassAppBug, "deterministic crash/render failure reproducing across attempts"
	}
	if matchAny(msg, mswMismatchPatterns) {
		return ClassTestGap, "MSW mock mismatch: spec exercises an endpoint the fixtures do not mock"
	}
	if matchAny(msg, assertionPatterns) {
		return ClassTestGap, "assertion failure with clean MSW interaction: spec vs fixture disagreement"
	}
	if reproducible {
		return ClassAppBug, "deterministic non-assertion failure reproducing across attempts"
	}
	return ClassTestGap, "unclassifiable failure; treated as wrong-but-stable expectation"
}

// reproducibleAcrossAttempts reports whether every recorded attempt that has an
// outcome failed (failed/timedOut) the same way — i.e. the failure reproduces
// across retries (the app_bug signature). A single-attempt failure is not
// evidence of reproducibility.
func reproducibleAcrossAttempts(t TestEvidence) bool {
	attempted := 0
	for _, a := range t.Attempts {
		switch a.Status {
		case AttemptFailed, AttemptTimedOut:
			attempted++
		case AttemptPassed, AttemptSkipped, AttemptInterrupted:
			// A pass on retry means it did NOT reproduce.
			return false
		}
	}
	return attempted > 1
}

// firstError returns the first non-empty error message across attempts,
// earliest attempt first (the deterministic cause, not the final noise).
func firstError(t TestEvidence) string {
	for _, a := range t.Attempts {
		if a.Error != "" {
			return a.Error
		}
	}
	return ""
}

func lower(s string) string { return strings.ToLower(s) }

func matchAny(msg string, patterns []string) bool {
	if msg == "" {
		return false
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
