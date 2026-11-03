package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pixcore/pixcore/services/ledger/internal/testutil"
)

// Defesa em profundidade: mesmo com o papel da aplicação (ou do dono), o banco rejeita
// mutações que violariam o ledger.
func TestDatabaseEnforcesLedgerInvariants(t *testing.T) {
	store, owner := testutil.NewStore(t)
	ctx := context.Background()
	app := store.Pool()

	a, b := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{a, b} {
		if _, err := app.Exec(ctx, `INSERT INTO ledger_account (id, external_ref, account_type, normal_side, currency)
			VALUES ($1,$2,'CUSTOMER','CREDIT','BRL')`, id, []string{"a", "b"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("unbalanced journal rejected at commit", func(t *testing.T) {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		jid := uuid.New()
		_, _ = tx.Exec(ctx, `INSERT INTO journal (id, kind, reference_id) VALUES ($1,'EXTERNAL_CREDIT','ub')`, jid)
		_, _ = tx.Exec(ctx, `INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'CREDIT',10,'BRL',0)`, uuid.New(), jid, a)
		_, _ = tx.Exec(ctx, `INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'DEBIT',9,'BRL',0)`, uuid.New(), jid, b)
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("unbalanced journal committed")
		}
	})

	// Débito em conta CUSTOMER sem saldo viola o CHECK.
	jid := uuid.New()
	tx, _ := app.Begin(ctx)
	_, _ = tx.Exec(ctx, `INSERT INTO journal (id, kind, reference_id) VALUES ($1,'EXTERNAL_CREDIT','ok')`, jid)

	t.Run("overdraft rejected by CHECK", func(t *testing.T) {
		_, err := tx.Exec(ctx, `INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'DEBIT',10,'BRL',0)`, uuid.New(), jid, a)
		if err == nil || !strings.Contains(err.Error(), "balance_non_negative") {
			t.Fatalf("expected balance_non_negative violation, got %v", err)
		}
	})
	_ = tx.Rollback(ctx)

	// Cria um lançamento legítimo com o dono para ter dados: reserva (debit) -> a (credit).
	res := uuid.New()
	if _, err := app.Exec(ctx, `INSERT INTO ledger_account (id, external_ref, account_type, normal_side, currency, allow_negative)
		VALUES ($1,'res','RESERVE','DEBIT','BRL',true)`, res); err != nil {
		t.Fatal(err)
	}
	tx, _ = app.Begin(ctx)
	jid = uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO journal (id, kind, reference_id) VALUES ($1,'EXTERNAL_CREDIT','good')`, []any{jid}},
		{`INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'DEBIT',50,'BRL',0)`, []any{uuid.New(), jid, res}},
		{`INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'CREDIT',50,'BRL',0)`, []any{uuid.New(), jid, a}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var bal int64
	if err := app.QueryRow(ctx, `SELECT balance FROM ledger_account WHERE id=$1`, a).Scan(&bal); err != nil || bal != 50 {
		t.Fatalf("trigger must apply balance: %d %v", bal, err)
	}

	deny := func(name, sql string, args ...any) {
		t.Run(name, func(t *testing.T) {
			if _, err := app.Exec(ctx, sql, args...); err == nil {
				t.Fatalf("app role executed forbidden statement: %s", sql)
			}
		})
	}
	deny("app cannot update entry", `UPDATE entry SET amount = 1`)
	deny("app cannot delete entry", `DELETE FROM entry`)
	deny("app cannot update journal", `UPDATE journal SET reference_id = 'x'`)
	deny("app cannot delete journal", `DELETE FROM journal`)
	deny("app cannot truncate entry", `TRUNCATE entry`)
	deny("app cannot set balance", `UPDATE ledger_account SET balance = 1000000 WHERE id = $1`, a)
	deny("app cannot change currency", `UPDATE ledger_account SET currency = 'USD' WHERE id = $1`, a)
	deny("app cannot delete account", `DELETE FROM ledger_account WHERE id = $1`, a)
	deny("app cannot rewrite command log", `UPDATE command_log SET outcome = 'SUCCESS'`)

	t.Run("owner is also blocked by triggers", func(t *testing.T) {
		for _, sql := range []string{`UPDATE entry SET amount = 1`, `DELETE FROM journal`, `TRUNCATE entry`, `UPDATE ledger_account SET balance = 9`} {
			if _, err := owner.Exec(ctx, sql); err == nil {
				t.Fatalf("owner executed: %s", sql)
			}
		}
	})

	t.Run("cannot append entries to an old journal", func(t *testing.T) {
		tx, _ := app.Begin(ctx)
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after) VALUES ($1,$2,$3,'CREDIT',1,'BRL',0)`, uuid.New(), jid, a); err == nil {
			t.Fatal("entry appended to a committed journal")
		}
	})

	t.Run("duplicate journal per reference and kind rejected", func(t *testing.T) {
		if _, err := app.Exec(ctx, `INSERT INTO journal (id, kind, reference_id) VALUES ($1,'EXTERNAL_CREDIT','good')`, uuid.New()); err == nil {
			t.Fatal("duplicate accepted")
		}
	})

	t.Run("hold state machine closed", func(t *testing.T) {
		h := uuid.New()
		if _, err := app.Exec(ctx, `INSERT INTO hold (id, reference_id, account_id, amount, currency, status) VALUES ($1,'hx',$2,5,'BRL','RELEASED')`, h, a); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `UPDATE hold SET status='HELD' WHERE id=$1`, h); err == nil {
			t.Fatal("RELEASED -> HELD allowed")
		}
	})

	t.Run("schema check and checksum", func(t *testing.T) {
		if err := store.CheckSchema(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
