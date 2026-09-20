-- 0005_mobile.sql — mobile surfaces (Phase 4, T4).
--
-- runs.surface pins the mobile surface a run targets. The default 'web'
-- preserves every pre-phase-4 run: they were all web-surface executions
-- before this column existed, and the worker/env contract (E2AI_SURFACE)
-- only ever needs a value, never a distinction between "old" and "unset".
-- Vocabulary is locked with the TS side: web | android | ios, lowercase.
alter table runs
  add column surface text not null default 'web'
  check (surface in ('web', 'android', 'ios'));
