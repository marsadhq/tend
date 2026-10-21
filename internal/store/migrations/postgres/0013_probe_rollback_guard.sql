-- 0013_probe_rollback_guard: the command column of a probe or a drill holds a
-- fixed command that fails (Postgres dialect), so a Tend older than v0.5, which runs a job of
-- a type it does not know as a shell job, reports these jobs as failed instead
-- of running nothing and reporting success. v0.5 writes it on every insert and
-- update (jobs.RollbackGuardCommand); this fills it in for rows written before.
UPDATE jobs SET command = 'echo "tend: this job needs tend v0.5 or later; it was not run" >&2; exit 2'
WHERE type IN ('tls', 'domain', 'expiry', 'drill');
