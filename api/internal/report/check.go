// T10: neutral check-run finalization. The runner created the check run as
// in_progress (execute stage) and stored runs.check_run_id; the report job
// patches it completed with an ALWAYS-neutral conclusion (report-only
// invariant: the check run reports evidence, it never gates on verdict).
package report

import (
	"context"
	"fmt"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/verdict"
)

// publishCheckRun patches the run's check run to completed/neutral with a
// summary + details_url pointing at the signed report link. No stored
// check-run id → skip (the create is the runner's job; both the runner
// create failing and non-dispatch flows land here, a no-op, never an
// error).
func (p *Publisher) publishCheckRun(ctx context.Context, facts *runFacts, link string) error {
	if !facts.CheckRunID.Valid {
		p.logger.Info("report: no check run to finalize", "head_sha", facts.HeadSHA)
		return nil
	}
	patch := github.UpdateCheckRunInput{
		Status:     "completed",
		Conclusion: "neutral",
		Output: &github.CheckRunOutput{
			Title:      "e2ai — " + string(facts.Rep.Verdict),
			Summary:    checkRunSummary(facts.Rep),
			DetailsURL: link,
		},
	}
	err := p.gh.UpdateCheckRun(ctx, facts.App.Owner, facts.App.Repo, facts.CheckRunID.Int64, patch)
	if err != nil {
		return fmt.Errorf("update check run %d: %w", facts.CheckRunID.Int64, err)
	}
	return nil
}

// checkRunSummary renders the check-run output summary from the verdict
// counts (skipped folds the not-evaluable tests: T9 vocabulary).
func checkRunSummary(rep verdict.Report) string {
	return fmt.Sprintf("%d passed, %d failed, %d flaky, %d skipped — full report in the details link",
		rep.Counts.Pass, rep.Counts.Fail, rep.Counts.Flaky,
		rep.Counts.Inconclusive+rep.Counts.InfraError+rep.Counts.InfraFlake)
}
