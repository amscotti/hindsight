package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// serverSecretKey is the app_meta row holding the HMAC secret used for
// signed cookies. The raw value never leaves the server.
const serverSecretKey = "secret"

var (
	gooseSetupOnce sync.Once
)

func setupGoose() error {
	var err error
	gooseSetupOnce.Do(func() {
		if e := goose.SetDialect("sqlite3"); e != nil {
			err = e
			return
		}
		goose.SetBaseFS(migrationsFS)
	})
	return err
}

// Open opens the SQLite file at path, applies the required PRAGMAs,
// runs the embedded goose migrations, and ensures the server secret
// exists, returning the handle and the secret together so callers do
// not need a second read. The modernc driver registers under the name "sqlite".
func Open(path string) (*sql.DB, string, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open sqlite: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sqldb.PingContext(ctx); err != nil {
		_ = sqldb.Close()
		return nil, "", fmt.Errorf("ping sqlite: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := sqldb.ExecContext(ctx, pragma); err != nil {
			_ = sqldb.Close()
			return nil, "", fmt.Errorf("apply %s: %w", pragma, err)
		}
	}

	if err := setupGoose(); err != nil {
		_ = sqldb.Close()
		return nil, "", fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(sqldb, "migrations"); err != nil {
		_ = sqldb.Close()
		return nil, "", fmt.Errorf("migrate: %w", err)
	}

	secret, err := LoadOrCreateServerSecret(ctx, sqldb)
	if err != nil {
		_ = sqldb.Close()
		return nil, "", err
	}
	return sqldb, secret, nil
}

// LoadOrCreateServerSecret returns the 256-bit server secret stored in
// app_meta, creating and persisting it on first use.
func LoadOrCreateServerSecret(ctx context.Context, sqldb *sql.DB) (string, error) {
	var value string
	err := sqldb.QueryRowContext(ctx,
		`SELECT value FROM app_meta WHERE key = ?`, serverSecretKey).Scan(&value)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate server secret: %w", err)
		}
		value = hex.EncodeToString(raw)
		if _, err := sqldb.ExecContext(ctx,
			`INSERT OR IGNORE INTO app_meta (key, value) VALUES (?, ?)`,
			serverSecretKey, value); err != nil {
			return "", fmt.Errorf("store server secret: %w", err)
		}
		if err := sqldb.QueryRowContext(ctx,
			`SELECT value FROM app_meta WHERE key = ?`, serverSecretKey).Scan(&value); err != nil {
			return "", fmt.Errorf("read server secret: %w", err)
		}
	default:
		return "", fmt.Errorf("read server secret: %w", err)
	}

	return value, nil
}
