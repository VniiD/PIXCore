package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

const relayLockID = 7_340_002

type OutboxMessage struct {
	ID      int64
	Topic   string
	Key     string
	Payload []byte
	Headers map[string]string
}

// RelayBatch publica até `limit` mensagens pendentes em ordem de id. Apenas um relay
// por vez (advisory lock) para preservar a ordem por chave. Entrega é at-least-once.
func (s *Store) RelayBatch(ctx context.Context, limit int, publish func(context.Context, []OutboxMessage) error) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var got bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLockID).Scan(&got); err != nil {
		return 0, err
	}
	if !got {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, topic, msg_key, payload, headers FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var msgs []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var h []byte
		if err := rows.Scan(&m.ID, &m.Topic, &m.Key, &m.Payload, &h); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal(h, &m.Headers); err != nil {
			rows.Close()
			return 0, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	if err := publish(ctx, msgs); err != nil {
		return 0, err
	}
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	return len(msgs), tx.Commit(ctx)
}

// OutboxLag devolve a quantidade pendente e a idade da mensagem mais antiga.
func (s *Store) OutboxLag(ctx context.Context) (pending int64, oldest time.Duration, err error) {
	var secs float64
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at)), 0) FROM outbox WHERE published_at IS NULL`).
		Scan(&pending, &secs)
	return pending, time.Duration(secs * float64(time.Second)), err
}

// Purge remove mensagens publicadas e registros de inbox antigos.
func (s *Store) Purge(ctx context.Context, olderThan time.Duration) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM outbox WHERE published_at IS NOT NULL AND published_at < now() - $1::interval`, olderThan.String()); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM inbox WHERE processed_at < now() - $1::interval`, olderThan.String())
	return err
}
