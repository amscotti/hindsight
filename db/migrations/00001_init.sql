-- +goose Up
-- Base schema: boards, columns, cards, votes, comments, kudos wall,
-- action items, participants, and the app_meta key/value store that
-- holds the server secret. Up-only; goose tracks versioning itself.

CREATE TABLE app_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE boards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  context TEXT NOT NULL DEFAULT '',
  facilitator_token_hash TEXT NOT NULL,
  phase TEXT NOT NULL DEFAULT 'collect'
    CHECK (phase IN ('collect', 'vote', 'discuss', 'done')),
  votes_per_person INTEGER NOT NULL DEFAULT 3
    CHECK (votes_per_person > 0),
  timer_ends_at TEXT NULL,
  voting_locked INTEGER NOT NULL DEFAULT 0,
  cards_locked INTEGER NOT NULL DEFAULT 0,
  cards_hidden INTEGER NOT NULL DEFAULT 0,
  focused_card_id INTEGER NULL,
  archived_at TEXT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE participants (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (board_id, name)
);
CREATE INDEX idx_participants_board_id ON participants (board_id);

CREATE TABLE columns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  color TEXT NOT NULL DEFAULT '',
  position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0)
);
CREATE INDEX idx_columns_board_id ON columns (board_id);

CREATE TABLE cards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  column_id INTEGER NOT NULL REFERENCES columns (id) ON DELETE CASCADE,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  body TEXT NOT NULL,
  author_name TEXT NOT NULL DEFAULT '',
  votes INTEGER NOT NULL DEFAULT 0 CHECK (votes >= 0),
  group_id INTEGER NULL REFERENCES cards (id) ON DELETE SET NULL,
  discussed INTEGER NOT NULL DEFAULT 0,
  position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_cards_column_id ON cards (column_id);
CREATE INDEX idx_cards_board_id ON cards (board_id);
CREATE INDEX idx_cards_group_id ON cards (group_id);

CREATE TABLE votes (
  participant_id INTEGER NOT NULL REFERENCES participants (id) ON DELETE CASCADE,
  card_id INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  PRIMARY KEY (participant_id, card_id)
);
CREATE INDEX idx_votes_participant_id ON votes (participant_id);
CREATE INDEX idx_votes_card_id ON votes (card_id);

CREATE TABLE comments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  card_id INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  body TEXT NOT NULL,
  author_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_comments_card_id ON comments (card_id);
CREATE INDEX idx_comments_board_id ON comments (board_id);

CREATE TABLE kudos (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  recipient TEXT NOT NULL,
  body TEXT NOT NULL,
  sender TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_kudos_board_id ON kudos (board_id);

CREATE TABLE actions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  done INTEGER NOT NULL DEFAULT 0,
  carried_from INTEGER NULL REFERENCES actions (id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_actions_board_id ON actions (board_id);
CREATE INDEX idx_actions_carried_from ON actions (carried_from);
