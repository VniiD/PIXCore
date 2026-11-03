// Package postgres implementa o armazenamento do ledger sobre PostgreSQL (pgx).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixcore/pixcore/services/ledger/internal/domain"
	"github.com/pixcore/pixcore/services/ledger/internal/ports"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ ports.Store = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func isRetryable(err error) bool {
	c := pgCode(err)
	return c == "40001" || c == "40P01"
}

const maxTxAttempts = 4

func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx ports.Tx) error) error {
	var err error
	for attempt := 0; attempt < maxTxAttempts; attempt++ {
		err = s.runTx(ctx, fn)
		if err == nil || !isRetryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
	return err
}

func (s *Store) runTx(ctx context.Context, fn func(ctx context.Context, tx ports.Tx) error) error {
	pt, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = pt.Rollback(ctx) }()
	if err := fn(ctx, &pgTx{tx: pt}); err != nil {
		return err
	}
	return pt.Commit(ctx)
}

const accountCols = `id, external_ref, account_type, currency, allow_negative, status, balance, version, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanAccount(r scanner) (domain.Account, error) {
	var a domain.Account
	var t, st string
	if err := r.Scan(&a.ID, &a.ExternalRef, &t, &a.Currency, &a.AllowNegative, &st, &a.Balance, &a.Version, &a.CreatedAt); err != nil {
		return domain.Account{}, err
	}
	a.Type, a.Status = domain.AccountType(t), domain.AccountStatus(st)
	return a, nil
}

func (s *Store) CreateAccount(ctx context.Context, a domain.Account) (domain.Account, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO ledger_account (id, external_ref, account_type, normal_side, currency, allow_negative, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+accountCols,
		a.ID, a.ExternalRef, string(a.Type), string(a.Type.NormalSide()), a.Currency, a.AllowNegative, string(a.Status))
	out, err := scanAccount(row)
	if pgCode(err) == "23505" {
		return domain.Account{}, domain.ErrConflict
	}
	return out, err
}

func (s *Store) getAccount(ctx context.Context, where string, arg any) (domain.Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM ledger_account WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrNotFound
	}
	return a, err
}

func (s *Store) GetAccountByID(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return s.getAccount(ctx, "id = $1", id)
}

func (s *Store) GetAccountByRef(ctx context.Context, ref string) (domain.Account, error) {
	return s.getAccount(ctx, "external_ref = $1", ref)
}

func (s *Store) SetAccountStatus(ctx context.Context, id uuid.UUID, st domain.AccountStatus) (domain.Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx,
		`UPDATE ledger_account SET status = $2 WHERE id = $1 RETURNING `+accountCols, id, string(st)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrNotFound
	}
	if pgCode(err) == "23000" {
		return domain.Account{}, fmt.Errorf("%w: %v", domain.ErrConflict, err)
	}
	return a, err
}

func (s *Store) GetBalance(ctx context.Context, id uuid.UUID) (domain.Balance, error) {
	var b domain.Balance
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.currency, a.balance,
		       COALESCE((SELECT SUM(h.amount) FROM hold h WHERE h.account_id = a.id AND h.status = 'HELD'), 0)::bigint
		FROM ledger_account a WHERE a.id = $1`, id).Scan(&b.AccountID, &b.Currency, &b.Available, &b.Held)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Balance{}, domain.ErrNotFound
	}
	return b, err
}

func (s *Store) ListEntries(ctx context.Context, id uuid.UUID, afterSeq int64, limit int) ([]domain.Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.seq, e.journal_id, j.kind, j.reference_id, e.account_id, e.direction, e.amount,
		       e.currency, e.balance_after, e.created_at
		FROM entry e JOIN journal j ON j.id = e.journal_id
		WHERE e.account_id = $1 AND e.seq > $2 ORDER BY e.seq LIMIT $3`, id, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Entry
	for rows.Next() {
		var e domain.Entry
		var kind, dir string
		if err := rows.Scan(&e.ID, &e.Seq, &e.JournalID, &kind, &e.ReferenceID, &e.AccountID, &dir,
			&e.Amount, &e.Currency, &e.BalanceAfter, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.JournalKind, e.Direction = domain.JournalKind(kind), domain.Direction(dir)
		out = append(out, e)
	}
	return out, rows.Err()
}

type pgTx struct{ tx pgx.Tx }

var _ ports.Tx = (*pgTx)(nil)

func (t *pgTx) LockReference(ctx context.Context, ref string) error {
	_, err := t.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, ref)
	return err
}

type storedResult struct {
	Reason string       `json:"reason,omitempty"`
	Event  domain.Event `json:"event"`
}

func (t *pgTx) GetCommand(ctx context.Context, ref string, cmd domain.Command) (*domain.CommandRecord, error) {
	var rec domain.CommandRecord
	var outcome string
	var raw []byte
	err := t.tx.QueryRow(ctx, `SELECT request_hash, outcome, result, journal_id FROM command_log WHERE reference_id=$1 AND command=$2`,
		ref, string(cmd)).Scan(&rec.RequestHash, &outcome, &raw, &rec.JournalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sr storedResult
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, err
	}
	rec.ReferenceID, rec.Command, rec.Outcome, rec.Reason, rec.Event = ref, cmd, domain.Outcome(outcome), sr.Reason, sr.Event
	return &rec, nil
}

func (t *pgTx) RecordCommand(ctx context.Context, rec domain.CommandRecord) error {
	raw, err := json.Marshal(storedResult{Reason: rec.Reason, Event: rec.Event})
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO command_log (reference_id, command, request_hash, outcome, result, journal_id)
		VALUES ($1,$2,$3,$4,$5,$6)`, rec.ReferenceID, string(rec.Command), rec.RequestHash, string(rec.Outcome), raw, rec.JournalID)
	return err
}

func (t *pgTx) LockAccounts(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]domain.Account, error) {
	rows, err := t.tx.Query(ctx, `SELECT `+accountCols+` FROM ledger_account WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]domain.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func (t *pgTx) SystemAccount(ctx context.Context, typ domain.AccountType, currency string) (domain.Account, error) {
	a, err := scanAccount(t.tx.QueryRow(ctx, `SELECT `+accountCols+` FROM ledger_account WHERE external_ref = $1`, domain.SystemRef(typ, currency)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, fmt.Errorf("system account %s/%s not provisioned", typ, currency)
	}
	return a, err
}

func (t *pgTx) InsertJournal(ctx context.Context, j domain.Journal) error {
	meta := j.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	mraw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, `INSERT INTO journal (id, kind, reference_id, metadata) VALUES ($1,$2,$3,$4)`,
		j.ID, string(j.Kind), j.ReferenceID, mraw); err != nil {
		return err
	}
	for _, l := range j.Legs {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := t.tx.Exec(ctx, `INSERT INTO entry (id, journal_id, account_id, direction, amount, currency, balance_after)
			VALUES ($1,$2,$3,$4,$5,$6,0)`, id, j.ID, l.AccountID, string(l.Direction), l.Amount, j.Currency); err != nil {
			return err
		}
	}
	return nil
}

func (t *pgTx) GetHold(ctx context.Context, ref string) (*domain.Hold, error) {
	var h domain.Hold
	var st string
	err := t.tx.QueryRow(ctx, `SELECT id, reference_id, account_id, journal_id, amount, currency, status FROM hold WHERE reference_id = $1 FOR UPDATE`, ref).
		Scan(&h.ID, &h.ReferenceID, &h.AccountID, &h.JournalID, &h.Amount, &h.Currency, &st)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	h.Status = domain.HoldStatus(st)
	return &h, err
}

func (t *pgTx) InsertHold(ctx context.Context, h domain.Hold) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO hold (id, reference_id, account_id, journal_id, amount, currency, status) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		h.ID, h.ReferenceID, h.AccountID, h.JournalID, h.Amount, h.Currency, string(h.Status))
	return err
}

func (t *pgTx) SetHoldStatus(ctx context.Context, id uuid.UUID, st domain.HoldStatus) error {
	ct, err := t.tx.Exec(ctx, `UPDATE hold SET status = $2, updated_at = now() WHERE id = $1`, id, string(st))
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (t *pgTx) Enqueue(ctx context.Context, m ports.OutboxMessage) error {
	h := m.Headers
	if h == nil {
		h = map[string]string{}
	}
	hraw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO outbox (aggregate_id, topic, msg_key, payload, headers) VALUES ($1,$2,$3,$4,$5)`,
		m.AggregateID, m.Topic, m.Key, m.Payload, hraw)
	return err
}

func (t *pgTx) MarkInbox(ctx context.Context, messageID, consumer string) (bool, error) {
	ct, err := t.tx.Exec(ctx, `INSERT INTO inbox (message_id, consumer) VALUES ($1,$2) ON CONFLICT DO NOTHING`, messageID, consumer)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}
