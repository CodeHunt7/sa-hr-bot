-- +goose Up

ALTER TABLE question_bank DROP CONSTRAINT question_bank_topic_check;
ALTER TABLE question_bank ADD CONSTRAINT question_bank_topic_check
    CHECK (topic IN (
        'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача', 'soft-skills'
    ));

ALTER TABLE sessions DROP CONSTRAINT sessions_next_topic_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_next_topic_check
    CHECK (next_topic IN (
        '', '*', 'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача', 'soft-skills'
    ));

-- +goose Down

UPDATE sessions
SET next_topic = ''
WHERE next_topic = 'soft-skills';

UPDATE question_bank
SET topic = 'подача', active = false
WHERE topic = 'soft-skills';

ALTER TABLE sessions DROP CONSTRAINT sessions_next_topic_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_next_topic_check
    CHECK (next_topic IN (
        '', '*', 'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача'
    ));

ALTER TABLE question_bank DROP CONSTRAINT question_bank_topic_check;
ALTER TABLE question_bank ADD CONSTRAINT question_bank_topic_check
    CHECK (topic IN (
        'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача'
    ));
