// Package db owns SQLite persistence: goose-versioned schema plus the
// transactional store. Every mutation runs in WithTx (BEGIN IMMEDIATE).
package db
