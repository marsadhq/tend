-- 0014_job_streaks: a job's failing streak, kept on the job: how many runs in
-- a row have not succeeded (failed, timed out or reaped; 0 while it
-- succeeds) and when the first of them was recorded (NULL while it
-- succeeds). Each run.failed adds one and records the streak in its detail; a
-- success starts over. Alerting reads it to tell the first failure of a
-- streak from the ones after it.
ALTER TABLE jobs ADD COLUMN failed_runs INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN failing_since TEXT;
