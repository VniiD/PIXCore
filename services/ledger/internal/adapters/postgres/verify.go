package postgres

import (
	"context"
	"time"
)

// Report reúne as invariantes monitoradas (matriz de falhas). Qualquer contador > 0 é incidente.
type Report struct {
	UnbalancedJournals int64
	BalanceMismatches  int64
	CurrencyImbalances int64
	StaleHolds         int64
}

func (r Report) Violations() int64 {
	return r.UnbalancedJournals + r.BalanceMismatches + r.CurrencyImbalances
}

func (s *Store) Verify(ctx context.Context, maxHoldAge time.Duration) (Report, error) {
	var r Report
	q := []struct {
		sql  string
		dst  *int64
		args []any
	}{
		{`SELECT count(*) FROM (SELECT 1 FROM entry GROUP BY journal_id
			HAVING SUM(CASE WHEN direction='DEBIT' THEN amount ELSE -amount END) <> 0) x`, &r.UnbalancedJournals, nil},
		{`SELECT count(*) FROM ledger_account a WHERE a.balance <> COALESCE(
			(SELECT SUM(CASE WHEN e.direction = a.normal_side THEN e.amount ELSE -e.amount END) FROM entry e WHERE e.account_id = a.id), 0)`,
			&r.BalanceMismatches, nil},
		{`SELECT count(*) FROM (SELECT currency FROM ledger_account GROUP BY currency
			HAVING SUM(CASE WHEN normal_side='CREDIT' THEN balance ELSE -balance END) <> 0) x`, &r.CurrencyImbalances, nil},
		{`SELECT count(*) FROM hold WHERE status='HELD' AND created_at < now() - $1::interval`, &r.StaleHolds, []any{maxHoldAge.String()}},
	}
	for _, it := range q {
		if err := s.pool.QueryRow(ctx, it.sql, it.args...).Scan(it.dst); err != nil {
			return Report{}, err
		}
	}
	return r, nil
}

// Snapshot grava o saldo de fechamento do dia (base da conciliação contábil). Idempotente.
func (s *Store) Snapshot(ctx context.Context, day time.Time) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO balance_snapshot (account_id, day, balance, held)
		SELECT a.id, $1::date, a.balance,
		       COALESCE((SELECT SUM(h.amount) FROM hold h WHERE h.account_id = a.id AND h.status='HELD'), 0)
		FROM ledger_account a ON CONFLICT DO NOTHING`, day.Format("2006-01-02"))
	return ct.RowsAffected(), err
}
