-- +goose Up
ALTER TABLE question_bank
    ADD COLUMN active BOOLEAN NOT NULL DEFAULT true;

-- These questions are either outside the five approved SA topics or test
-- implementation details that belong primarily to developers/DBAs. Keep the
-- rows for historical question_attempt references, but never select them for
-- a new training block.
UPDATE question_bank
SET active = false
WHERE question_text IN (
    'Какие гарантии доставки дает Kafka и как выбрать нужную под задачу?',
    'Как ты оцениваешь сроки выполнения своей задачи?',
    'Какие архитектурные паттерны знаешь: SAGA, CQRS, BFF, API Gateway - и когда они нужны?',
    'Как вносить изменения в схему БД без простоя в продакшне?',
    'Как ты структурируешь ответ на технический вопрос на собеседовании?',
    'Расскажи о задаче, которую ты решал: контекст, что сделал, инструмент, результат.',
    'Что такое кэш и какие стратегии кэширования знаешь?',
    'Как ты работаешь с AI-инструментами в задачах аналитика?'
);

CREATE INDEX idx_question_bank_active_grade_topic
    ON question_bank (active, grade, topic);

-- +goose Down
DROP INDEX idx_question_bank_active_grade_topic;
ALTER TABLE question_bank DROP COLUMN active;
