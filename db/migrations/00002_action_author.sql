-- +goose Up
-- Record who created each action item, so later edits can tell the
-- author apart from other participants. Existing rows keep an empty
-- author; carry-over copies preserve the source author.
ALTER TABLE actions ADD COLUMN author_name TEXT NOT NULL DEFAULT '';
