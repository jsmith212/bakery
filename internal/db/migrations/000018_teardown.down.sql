-- Reverse of 000018. Dropping the columns takes their partial indexes with them.
--
-- A row still marked deleting when this runs becomes LIVE again, which is the only
-- honest rollback: the objects the teardown stage had not reached yet are still
-- there, the backend still resolves, and an operator who rolls back mid-teardown
-- gets a working backend rather than a half-swept one nothing can reach.
ALTER TABLE projects       DROP COLUMN deleting_at;
ALTER TABLE cache_backends DROP COLUMN deleting_at;
