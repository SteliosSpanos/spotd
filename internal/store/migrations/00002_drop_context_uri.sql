-- FIX: context_uri was never populated, drop it
-- +goose Up
ALTER TABLE plays DROP COLUMN context_uri;

-- +goose Down
ALTER TABLE plays ADD COLUMN context_uri TEXT;
