-- +goose Up

-- Keep existing registrations and their original references intact. New
-- participants register by Telegram ID after the channel check.
ALTER TABLE students ALTER COLUMN access_code DROP NOT NULL;

-- +goose Down

-- PostgreSQL refuses this rollback if the database has registrations from
-- this branch. The existing participant rows must be preserved.
ALTER TABLE students ALTER COLUMN access_code SET NOT NULL;
