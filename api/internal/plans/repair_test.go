// Repair-proposal review surface tests (FLAK-02): the queue list, the
// attributed one-shot decide endpoint, and the report-only invariant at the
// REST boundary — approve is the only mutation, and it goes through the same
// promote transaction the manual flow uses.
package plans

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func repairQueueCall(t *testing.T, srv string, method, url, body string) (int, []map[string]any, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv+url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	var one map[string]any
	_ = json.Unmarshal(raw, &one)
	return resp.StatusCode, list, one
}

func seedRepairProposal(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) *corpus.RepairProposal {
	t.Helper()
	ctx := context.Background()
	var learningID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into corpus_learnings (app_id, observation)
		values ($1, 'deposit button locator went stale after the v2 deploy')
		returning id`, appID).Scan(&learningID); err != nil {
		t.Fatalf("seed learning: %v", err)
	}
	p, err := corpus.AppendRepairProposal(ctx, pool, corpus.RepairProposal{
		LearningID:       learningID,
		AppID:            appID,
		BehaviorKey:      "LEARN-X",
		BrokenSelector:   "css=#deposit-v1",
		ProposedSelector: `role=button[name="Deposit"]`,
		Rationale:        "a11y snapshot shows an accessible-name button",
		Confidence:       0.9,
		EvidenceRef:      "runs/r/step-3/a11y.json",
		ProposedBy:       "navigator",
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return p
}

func TestRepairReviewQueueAndDecide(t *testing.T) {
	pool := plansTestPool(t)
	appID := plansTestApp(t, pool)
	p := seedRepairProposal(t, pool, appID)
	srv := serve(t, NewPlansHTTP(pool, nil))

	// Queue listing: the proposal row with the additive wire shape.
	code, list, _ := repairQueueCall(t, srv.URL, "GET", "/api/corpus/repairs?app="+uuid.UUID(appID.Bytes).String(), "")
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("queue = %d %v, want one proposed row", code, list)
	}
	row := list[0]
	for _, key := range []string{"id", "learning_id", "app_id", "behavior_key", "broken_selector",
		"proposed_selector", "rationale", "confidence", "evidence_ref", "status", "proposed_by", "decided_by", "created_at"} {
		if _, has := row[key]; !has {
			t.Errorf("queue row missing %q", key)
		}
	}
	if row["status"] != "proposed" {
		t.Fatalf("queue status = %v, want proposed", row["status"])
	}

	// Approve: attributed + one-shot; the ONLY path that mutates the oracle.
	decide := func(decision string) (int, map[string]any) {
		code, _, one := repairQueueCall(t, srv.URL, "POST",
			"/api/corpus/repairs/"+p.ID.String()+"/decide", `{"decision":"`+decision+`"}`)
		return code, one
	}
	code, body := decide("approve")
	if code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("approve = %d %v, want approved", code, body)
	}
	if body["decided_by"] != "you" {
		t.Fatalf("decided_by = %v, want the identity-wall fallback user", body["decided_by"])
	}

	// The approval promoted the learning into a behavior — the human-gated mutation.
	var behaviorKey string
	var verifiedBy pgtype.Text
	var verifiedAt pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(), `
		select key, verified_by, verified_at from corpus_behaviors where app_id = $1`,
		appID).Scan(&behaviorKey, &verifiedBy, &verifiedAt); err != nil {
		t.Fatalf("approval must promote the learning into a behavior: %v", err)
	}
	if verifiedBy.String != "you" || !verifiedAt.Valid {
		t.Fatalf("behavior verified_by/at = %q %v, want attributed fallback user", verifiedBy.String, verifiedAt)
	}
	var learningStatus string
	if err := pool.QueryRow(context.Background(),
		`select status from corpus_learnings where id = $1`, p.LearningID).Scan(&learningStatus); err != nil {
		t.Fatal(err)
	}
	if learningStatus != corpus.LearningPromoted {
		t.Fatalf("learning status = %q, want promoted", learningStatus)
	}

	// One-shot: a second decision is a 409.
	if code, _ := decide("reject"); code != http.StatusConflict {
		t.Fatalf("second decide = %d, want 409", code)
	}
}

func TestRepairDecideRejectLeavesOracleUntouched(t *testing.T) {
	pool := plansTestPool(t)
	appID := plansTestApp(t, pool)
	p := seedRepairProposal(t, pool, appID)
	srv := serve(t, NewPlansHTTP(pool, nil))

	code, _, one := repairQueueCall(t, srv.URL, "POST",
		"/api/corpus/repairs/"+p.ID.String()+"/decide", `{"decision":"reject"}`)
	if code != http.StatusOK || one["status"] != "rejected" {
		t.Fatalf("reject = %d %v, want rejected", code, one)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejection must not mutate the corpus oracle, behaviors = %d", n)
	}
	var learningStatus string
	if err := pool.QueryRow(context.Background(),
		`select status from corpus_learnings where id = $1`, p.LearningID).Scan(&learningStatus); err != nil {
		t.Fatal(err)
	}
	if learningStatus != corpus.LearningUnverified {
		t.Fatalf("learning status = %q, want unverified (untouched)", learningStatus)
	}
}

func TestRepairDecideValidation(t *testing.T) {
	pool := plansTestPool(t)
	appID := plansTestApp(t, pool)
	p := seedRepairProposal(t, pool, appID)
	srv := serve(t, NewPlansHTTP(pool, nil))

	// Unknown repair id → 404.
	code, _, _ := repairQueueCall(t, srv.URL, "POST",
		"/api/corpus/repairs/"+uuid.NewString()+"/decide", `{"decision":"approve"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown repair = %d, want 404", code)
	}
	// Invalid decision verb → 400, row still proposed.
	code, _, _ = repairQueueCall(t, srv.URL, "POST",
		"/api/corpus/repairs/"+p.ID.String()+"/decide", `{"decision":"self-approve"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid decision = %d, want 400", code)
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`select status from corpus_repair_proposals where id = $1`, p.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != corpus.RepairProposed {
		t.Fatalf("proposal status = %q, want still proposed", status)
	}

}
