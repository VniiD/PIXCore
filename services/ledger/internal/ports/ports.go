// Package ports define as interfaces de saída usadas pelos casos de uso.
package ports

import (
	"context"

	"github.com/google/uuid"

	"github.com/pixcore/pixcore/services/ledger/internal/domain"
)

// OutboxMessage é gravada na mesma transação do lançamento (ADR-0003).
type OutboxMessage struct {
	AggregateID string
	Topic       string
	Key         string
	Payload     []byte
	Headers     map[string]string
}

// Tx é a unidade de trabalho transacional.
type Tx interface {
	// LockReference serializa comandos concorrentes com o mesmo reference_id.
	LockReference(ctx context.Context, ref string) error
	GetCommand(ctx context.Context, ref string, cmd domain.Command) (*domain.CommandRecord, error)
	RecordCommand(ctx context.Context, rec domain.CommandRecord) error

	// LockAccounts faz SELECT ... FOR UPDATE em ordem determinística de id.
	// Contas inexistentes simplesmente não aparecem no resultado.
	LockAccounts(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]domain.Account, error)
	SystemAccount(ctx context.Context, t domain.AccountType, currency string) (domain.Account, error)

	InsertJournal(ctx context.Context, j domain.Journal) error

	GetHold(ctx context.Context, ref string) (*domain.Hold, error)
	InsertHold(ctx context.Context, h domain.Hold) error
	SetHoldStatus(ctx context.Context, id uuid.UUID, status domain.HoldStatus) error

	Enqueue(ctx context.Context, m OutboxMessage) error
	// MarkInbox devolve false se a mensagem já foi processada por este consumidor.
	MarkInbox(ctx context.Context, messageID, consumer string) (bool, error)
}

type Store interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	CreateAccount(ctx context.Context, a domain.Account) (domain.Account, error)
	GetAccountByID(ctx context.Context, id uuid.UUID) (domain.Account, error)
	GetAccountByRef(ctx context.Context, ref string) (domain.Account, error)
	SetAccountStatus(ctx context.Context, id uuid.UUID, s domain.AccountStatus) (domain.Account, error)
	GetBalance(ctx context.Context, id uuid.UUID) (domain.Balance, error)
	ListEntries(ctx context.Context, id uuid.UUID, afterSeq int64, limit int) ([]domain.Entry, error)
}
