-- 0015_job_streaks_backfill: a job that is failing when Tend is upgraded gets
-- the streak its stored runs show: the failed and timed-out runs that ended
-- after its last success (all of them when it never succeeded), dated from
-- the first of them, so its next failure counts on from there. Each job's
-- last success is found once (ok) and the runs are grouped once (s), so the
-- backfill is one pass over job_runs however long the history. Timestamps
-- are fixed-width UTC text, so they compare as text.
UPDATE jobs SET failed_runs = s.n, failing_since = s.since
FROM (
  SELECT r.org_id, r.job_id, COUNT(*) AS n, MIN(r.ended_at) AS since
    FROM job_runs r
    LEFT JOIN (
      SELECT org_id, job_id, MAX(ended_at) AS at FROM job_runs
       WHERE status = 'succeeded' AND ended_at IS NOT NULL
       GROUP BY org_id, job_id
    ) ok ON ok.org_id = r.org_id AND ok.job_id = r.job_id
   WHERE r.status IN ('failed', 'timed_out') AND r.ended_at IS NOT NULL
     AND r.ended_at > COALESCE(ok.at, '')
   GROUP BY r.org_id, r.job_id
) s
WHERE s.org_id = jobs.org_id AND s.job_id = jobs.id;
