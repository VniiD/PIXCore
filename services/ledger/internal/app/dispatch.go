package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/pixcore/pixcore/libs/platform/events"
	"github.com/pixcore/pixcore/services/ledger/internal/domain"
)

// ErrPermanent marca mensagens que jamais terão sucesso por reentrega (vão para a DLQ).
var ErrPermanent = errors.New("permanent message failure")

const Consumer = "ledger-commands"

type holdData struct {
	Reference string    `json:"reference_id"`
	AccountID uuid.UUID `json:"account_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
}

type refData struct {
	Reference string `json:"reference_id"`
}

type transferData struct {
	Reference string            `json:"reference_id"`
	From      uuid.UUID         `json:"from_account_id"`
	To        uuid.UUID         `json:"to_account_id"`
	Amount    int64             `json:"amount"`
	Currency  string            `json:"currency"`
	Metadata  map[string]string `json:"metadata"`
}

func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return nil
}

// HandleCommand processa um comando vindo do broker (já verificado/assinado pelo adapter).
func (s *Service) HandleCommand(ctx context.Context, env events.Envelope) error {
	ctx = WithDelivery(ctx, env.ID, Consumer)
	var err error
	switch env.Type {
	case "ledger.hold.requested":
		var d holdData
		if err = strict(env.Data, &d); err == nil {
			_, err = s.Hold(ctx, HoldCmd{d.Reference, d.AccountID, d.Amount, d.Currency})
		}
	case "ledger.settle.requested":
		var d refData
		if err = strict(env.Data, &d); err == nil {
			_, err = s.Settle(ctx, SettleCmd{d.Reference})
		}
	case "ledger.release.requested":
		var d refData
		if err = strict(env.Data, &d); err == nil {
			_, err = s.Release(ctx, ReleaseCmd{d.Reference})
		}
	case "ledger.transfer.requested":
		var d transferData
		if err = strict(env.Data, &d); err == nil {
			_, err = s.Transfer(ctx, TransferCmd{d.Reference, d.From, d.To, d.Amount, d.Currency, d.Metadata})
		}
	default:
		return fmt.Errorf("%w: unknown command type %q", ErrPermanent, env.Type)
	}
	if errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrIdempotencyMismatch) {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}
