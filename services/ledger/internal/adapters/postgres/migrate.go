package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLockID = 7_340_001

type migration struct {
	version  string
	sql      string
	checksum string
}

func loadMigrations() ([]migration, error) {
	ents, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range ents {
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		ms = append(ms, migration{strings.TrimSuffix(e.Name(), ".sql"), string(b), hex.EncodeToString(sum[:])})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// Migrate aplica migrações pendentes com o papel dono do schema e rejeita alteração
// retroativa de migrações já aplicadas (checksum).
func Migrate(ctx context.Context, conn *pgx.Conn) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, m := range ms {
		var sum string
		err := conn.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, m.version).Scan(&sum)
		if err == nil {
			if sum != m.checksum {
				return fmt.Errorf("migration %s was modified after being applied", m.version)
			}
			continue
		}
		if err != pgx.ErrNoRows {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1,$2)`, m.version, m.checksum); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ExpectedVersion devolve a última migração embutida (usada para checar o schema na subida).
func ExpectedVersion() (string, error) {
	ms, err := loadMigrations()
	if err != nil || len(ms) == 0 {
		return "", fmt.Errorf("no migrations: %v", err)
	}
	return ms[len(ms)-1].version, nil
}

// CheckSchema falha se o banco não está na versão esperada.
func (s *Store) CheckSchema(ctx context.Context) error {
	want, err := ExpectedVersion()
	if err != nil {
		return err
	}
	var got string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),'') FROM schema_migrations`).Scan(&got); err != nil {
		return fmt.Errorf("schema check: %w", err)
	}
	if got != want {
		return fmt.Errorf("schema version %q, expected %q (run `ledger-service migrate`)", got, want)
	}
	return nil
}
