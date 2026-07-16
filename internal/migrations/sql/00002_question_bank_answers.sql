-- +goose Up
ALTER TABLE question_bank DROP COLUMN reference_answer;
ALTER TABLE question_bank ADD COLUMN followup_1 TEXT NOT NULL DEFAULT '';
ALTER TABLE question_bank ADD COLUMN followup_2 TEXT NOT NULL DEFAULT '';
ALTER TABLE question_bank ADD COLUMN answer_junior TEXT NOT NULL DEFAULT '';
ALTER TABLE question_bank ADD COLUMN answer_middle TEXT NOT NULL DEFAULT '';
ALTER TABLE question_bank ADD COLUMN answer_senior TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE question_bank DROP COLUMN followup_1;
ALTER TABLE question_bank DROP COLUMN followup_2;
ALTER TABLE question_bank DROP COLUMN answer_junior;
ALTER TABLE question_bank DROP COLUMN answer_middle;
ALTER TABLE question_bank DROP COLUMN answer_senior;
ALTER TABLE question_bank ADD COLUMN reference_answer TEXT NOT NULL DEFAULT '';
