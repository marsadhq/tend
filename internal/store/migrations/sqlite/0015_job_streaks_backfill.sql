-- 0015_job_streaks_backfill: a job that is failing when Tend is upgraded gets
-- the streak its stored runs show: the failed and timed-out runs that ended
-- after its last success (all of them when it never succeeded), dated from
-- the first of them, so its next failure counts on from there. Timestamps are
-- fixed-width UTC text, so they compare as text.
UPDATE jobs SET
  failed_runs = (
    SELECT COUNT(*) FROM job_runs r
     WHERE r.org_id = jobs.org_id AND r.job_id = jobs.id
       AND r.status IN ('failed', 'timed_out') AND r.ended_at IS NOT NULL
       AND r.ended_at > COALESCE((
         SELECT MAX(s.ended_at) FROM job_runs s
          WHERE s.org_id = jobs.org_id AND s.job_id = jobs.id
            AND s.status = 'succeeded' AND s.ended_at IS NOT NULL), '')),
  failing_since = (
    SELECT MIN(r.ended_at) FROM job_runs r
     WHERE r.org_id = jobs.org_id AND r.job_id = jobs.id
       AND r.status IN ('failed', 'timed_out') AND r.ended_at IS NOT NULL
       AND r.ended_at > COALESCE((
         SELECT MAX(s.ended_at) FROM job_runs s
          WHERE s.org_id = jobs.org_id AND s.job_id = jobs.id
            AND s.status = 'succeeded' AND s.ended_at IS NOT NULL), ''));
