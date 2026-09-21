-- 0009_repair_proposals.sql — Phase 5 T7 (05-01-PLAN): FLAK-02 selector-repair
-- proposals attached to corpus learning records.
--
-- Report-only posture (RESEARCH decision 1): the navigator never rewrites
-- plans, tests, or the corpus. A failed locator act produces — at most — a
-- proposal row here, attached to the learning the failure generated. Adoption
-- is a human decision on this table; only an approval mutates the oracle, and
-- it does so through the exact same promote path the manual flow uses
-- (internal/corpus promoteLearningTx), so there is no second write path.
create table corpus_repair_proposals (
  id uuid primary key default gen_random_uuid(),
  learning_id uuid not null references corpus_learnings(id),
  app_id uuid not null references registered_apps(id),
  behavior_key text not null,            -- corpus behavior the broken selector served
  broken_selector text not null,
  proposed_selector text not null,
  rationale text not null,
  confidence numeric not null check (confidence >= 0 and confidence <= 1),
  evidence_ref text not null default '', -- run-relative artifact path
  status text not null default 'proposed' check (status in ('proposed','approved','rejected')),
  proposed_by text not null,
  decided_by text,                       -- null until a human decides (report-only)
  decided_at timestamptz,
  created_at timestamptz not null default now()
);
create index corpus_repair_proposals_app_idx on corpus_repair_proposals (app_id, created_at desc);
