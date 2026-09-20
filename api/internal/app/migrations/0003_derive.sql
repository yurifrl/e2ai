-- 0003_derive.sql — Phase 2 T8: the run-level plan resolution record.
-- Written by the execute worker (the resolution gate) and the derive
-- worker (draft derivation status); one row per run.
--
-- Shape per RESEARCH.md: mode is the gate's decision ('approved' = the
-- deploy ran approved plan versions, 'legacy_full_suite' = no plans at
-- all, Phase 1 behavior), resolution carries the full
-- {selected, excluded, diff_truncated} evidence blob the REST surface
-- reads, selected is the derived test subset actually executed, and the
-- plan columns pin the version + content hash that was approved.

create table run_plan_resolutions (
  run_id uuid primary key references runs(id) on delete cascade,
  plan_id text,
  plan_version_id uuid,
  content_hash text,
  selected jsonb not null default '[]',  -- derived test subset + reasons
  mode text not null check (mode in ('approved','legacy_full_suite','draft_only')),
  resolution jsonb not null default '{}', -- {selected[], excluded[], diff_truncated}
  recorded_at timestamptz not null default now()
);
