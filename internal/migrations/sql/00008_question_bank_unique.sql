-- +goose Up

-- Merge duplicate bank rows left by the old append-only importer. Preserve all
-- session/attempt references by pointing them at the oldest copy first.
WITH canonical AS (
    SELECT question_text, MIN(id) AS keep_id
    FROM question_bank
    GROUP BY question_text
)
UPDATE sessions s
SET current_question_id = c.keep_id
FROM question_bank q
JOIN canonical c ON c.question_text = q.question_text
WHERE s.current_question_id = q.id AND q.id <> c.keep_id;

WITH canonical AS (
    SELECT question_text, MIN(id) AS keep_id
    FROM question_bank
    GROUP BY question_text
)
UPDATE question_attempts a
SET question_id = c.keep_id
FROM question_bank q
JOIN canonical c ON c.question_text = q.question_text
WHERE a.question_id = q.id AND q.id <> c.keep_id;

DELETE FROM question_bank duplicate
USING question_bank canonical
WHERE duplicate.question_text = canonical.question_text
  AND duplicate.id > canonical.id;

CREATE UNIQUE INDEX idx_question_bank_question_text_unique
    ON question_bank (question_text);

-- +goose Down
DROP INDEX idx_question_bank_question_text_unique;
