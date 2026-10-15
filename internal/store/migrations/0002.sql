ALTER TABLE attempts ADD COLUMN at_ns INTEGER NOT NULL DEFAULT 0;

UPDATE attempts
SET at_ns = (SELECT at_ns FROM requests WHERE requests.id = attempts.request_id);

CREATE INDEX attempts_at_ns ON attempts(at_ns);
