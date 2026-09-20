-- 0006_runner_pinning.sql — Phase 4 T8 (PLAT-05): per-run runner image
-- pinning + runner-version persistence.
--
-- Two run-row columns with two distinct semantics (deliberately not one):
--   runs.runner_image   text — the container image THIS run's suite executed
--                       in: the process default pin (RUNNER_IMAGE) or a
--                       per-run override (the image canary). Stamped by the
--                       execute job before the container starts, so pins stay
--                       auditable even for runs that fail mid-flight. The
--                       candidate-vs-default comparison reads this column:
--                       `where runner_image = $candidate` vs everything else.
--   runs.runner_version text — the Playwright version the parsed report
--                       carries: the runner-code identity inside the image,
--                       the drift canary for the image pin. Null until the
--                       report parses (null also = legacy pre-pinning rows).
alter table runs
  add column runner_image text,
  add column runner_version text;
