-- 0012_job_spec: the settings of the job types that need more than a command
-- or a URL (tls, domain and expiry probes, restore drills), as JSON (Postgres
-- dialect). NULL for shell and http jobs, so every existing job reads back
-- exactly as before.
ALTER TABLE jobs ADD COLUMN spec TEXT;
