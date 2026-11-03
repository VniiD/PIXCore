// Package testutil cria bancos PostgreSQL isolados para testes de integração.
package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixcore/pixcore/services/ledger/internal/adapters/postgres"
)

const EnvDSN = "PIXCORE_TEST_DATABASE_URL"

// NewStore cria um banco novo, migra com o dono e devolve um Store que conecta como
// ledger_app (privilégios mínimos). Também devolve um pool com o papel dono.
func NewStore(t testing.TB) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s not set; skipping integration test", EnvDSN)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := "ledger_t_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, db)); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	mc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, mc); err != nil {
		t.Fatal(err)
	}
	_ = mc.Close(ctx)

	mk := func(role string) *pgxpool.Pool {
		pc, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		pc.ConnConfig.Database = db
		pc.MaxConns = 20
		if role != "" {
			pc.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
				_, err := c.Exec(ctx, "SET ROLE "+role)
				return err
			}
		}
		p, err := pgxpool.NewWithConfig(ctx, pc)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	app, owner := mk("ledger_app"), mk("")
	t.Cleanup(func() {
		app.Close()
		owner.Close()
		_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, db))
		_ = admin.Close(ctx)
	})
	return postgres.NewStore(app), owner
}
