// Package app implementa os casos de uso do ledger (TCC: Hold/Settle/Release, Transfer, Credit).
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/pixcore/pixcore/libs/platform/events"
	"github.com/pixcore/pixcore/services/ledger/internal/domain"
	"github.com/pixcore/pixcore/services/ledger/internal/ports"
)

const (
	TopicEvents   = "pixcore.ledger.events.v1"
	TopicCommands = "pixcore.ledger.commands.v1"
	eventSource   = "pixcore/ledger-service"
)

type Config struct {
	Currencies []string // moedas aceitas (allowlist)
}

type Service struct {
	store      ports.Store
	keyring    *events.Keyring
	currencies map[string]bool
}

func NewService(store ports.Store, keyring *events.Keyring, cfg Config) (*Service, error) {
	if len(cfg.Currencies) == 0 {
		return nil, errors.New("at least one currency is required")
	}
	cs := map[string]bool{}
	for _, c := range cfg.Currencies {
		if err := domain.ValidateCurrency(c); err != nil {
			return nil, err
		}
		cs[c] = true
	}
	return &Service{store: store, keyring: keyring, currencies: cs}, nil
}

type Result struct {
	Outcome   domain.Outcome
	Reason    string
	JournalID *uuid.UUID
	Replayed  bool
}

type delivery struct{ messageID, consumer string }
type deliveryKey struct{}

// WithDelivery associa o comando a uma mensagem do broker para deduplicação via inbox.
func WithDelivery(ctx context.Context, messageID, consumer string) context.Context {
	return context.WithValue(ctx, deliveryKey{}, delivery{messageID, consumer})
}

type HoldCmd struct {
	Reference string
	AccountID uuid.UUID
	Amount    int64
	Currency  string
}

type SettleCmd struct{ Reference string }
type ReleaseCmd struct{ Reference string }

type TransferCmd struct {
	Reference string
	From, To  uuid.UUID
	Amount    int64
	Currency  string
	Metadata  map[string]string
}

type CreditCmd struct {
	Reference string
	AccountID uuid.UUID
	Amount    int64
	Currency  string
	Metadata  map[string]string
}

func hashOf(parts ...any) string {
	b, _ := json.Marshal(parts) // map keys são ordenados pelo encoding/json
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

func eventType(cmd domain.Command, o domain.Outcome) string {
	ok := o != domain.OutcomeFailure
	switch cmd {
	case domain.CmdHold:
		if ok {
			return "ledger.hold.succeeded"
		}
		return "ledger.hold.failed"
	case domain.CmdSettle:
		if ok {
			return "ledger.settled"
		}
		return "ledger.settle.failed"
	case domain.CmdRelease:
		if ok {
			return "ledger.released"
		}
		return "ledger.release.failed"
	case domain.CmdTransfer:
		if ok {
			return "ledger.transferred"
		}
		return "ledger.transfer.failed"
	default:
		if ok {
			return "ledger.credited"
		}
		return "ledger.credit.failed"
	}
}

func (s *Service) enqueue(ctx context.Context, tx ports.Tx, ref string, ev domain.Event) error {
	id := ref + ":" + ev.Type
	env, err := events.New(id, ev.Type, eventSource, ref, ev.Data)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	h := map[string]string{}
	if sig := s.keyring.Sign(payload); sig != "" {
		h[events.SignatureHeader] = sig
	}
	return tx.Enqueue(ctx, ports.OutboxMessage{AggregateID: ref, Topic: TopicEvents, Key: ref, Payload: payload, Headers: h})
}

func resultOf(rec domain.CommandRecord, replayed bool) Result {
	return Result{Outcome: rec.Outcome, Reason: rec.Reason, JournalID: rec.JournalID, Replayed: replayed}
}

type runner func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error)

// execute aplica a idempotência lógica: sucesso e falha ficam gravados; reentrega
// devolve o resultado gravado e reemite o evento (ADR-0006).
func (s *Service) execute(ctx context.Context, cmd domain.Command, ref, hash string, run runner) (Result, error) {
	var res Result
	err := s.store.InTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if d, ok := ctx.Value(deliveryKey{}).(delivery); ok {
			first, err := tx.MarkInbox(ctx, d.messageID, d.consumer)
			if err != nil {
				return err
			}
			if !first {
				res = Result{Replayed: true}
				return nil
			}
		}
		if err := tx.LockReference(ctx, ref); err != nil {
			return err
		}
		prev, err := tx.GetCommand(ctx, ref, cmd)
		if err != nil {
			return err
		}
		if prev != nil {
			if prev.RequestHash != hash {
				return domain.ErrIdempotencyMismatch
			}
			res = resultOf(*prev, true)
			return s.enqueue(ctx, tx, ref, prev.Event)
		}
		rec, err := run(ctx, tx)
		if err != nil {
			return err
		}
		rec.ReferenceID, rec.Command, rec.RequestHash = ref, cmd, hash
		rec.Event.Type = eventType(cmd, rec.Outcome)
		if rec.Event.Data == nil {
			rec.Event.Data = map[string]any{}
		}
		rec.Event.Data["reference_id"] = ref
		rec.Event.Data["outcome"] = string(rec.Outcome)
		if rec.Reason != "" {
			rec.Event.Data["reason"] = rec.Reason
		}
		if rec.JournalID != nil {
			rec.Event.Data["journal_id"] = rec.JournalID.String()
		}
		if err := tx.RecordCommand(ctx, rec); err != nil {
			return err
		}
		res = resultOf(rec, false)
		return s.enqueue(ctx, tx, ref, rec.Event)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func failure(reason string, data map[string]any) (domain.CommandRecord, error) {
	return domain.CommandRecord{Outcome: domain.OutcomeFailure, Reason: reason, Event: domain.Event{Data: data}}, nil
}

func (s *Service) post(ctx context.Context, tx ports.Tx, kind domain.JournalKind, ref, currency string, meta map[string]string, legs ...domain.Leg) (uuid.UUID, error) {
	j := domain.Journal{ID: newID(), Kind: kind, ReferenceID: ref, Currency: currency, Metadata: meta, Legs: legs}
	if err := j.Validate(); err != nil {
		return uuid.Nil, err
	}
	if err := tx.InsertJournal(ctx, j); err != nil {
		return uuid.Nil, err
	}
	return j.ID, nil
}

func success(journal uuid.UUID, data map[string]any) (domain.CommandRecord, error) {
	return domain.CommandRecord{Outcome: domain.OutcomeSuccess, JournalID: &journal, Event: domain.Event{Data: data}}, nil
}

func (s *Service) checkCurrency(c string) error {
	if err := domain.ValidateCurrency(c); err != nil {
		return err
	}
	if !s.currencies[c] {
		return fmt.Errorf("%w: currency %s not enabled", domain.ErrInvalidArgument, c)
	}
	return nil
}

// Hold (Try): debita o cliente e credita o trânsito; o disponível já cai.
func (s *Service) Hold(ctx context.Context, c HoldCmd) (Result, error) {
	if err := firstErr(domain.ValidateReference(c.Reference), domain.ValidateAmount(c.Amount), s.checkCurrency(c.Currency)); err != nil {
		return Result{}, err
	}
	data := map[string]any{"account_id": c.AccountID.String(), "amount": c.Amount, "currency": c.Currency}
	return s.execute(ctx, domain.CmdHold, c.Reference, hashOf("HOLD", c.AccountID, c.Amount, c.Currency),
		func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error) {
			transit, err := tx.SystemAccount(ctx, domain.AccountSettlementTransit, c.Currency)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			accts, err := tx.LockAccounts(ctx, c.AccountID, transit.ID)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			cust, found := accts[c.AccountID]
			if !found {
				return failure(domain.ReasonAccountNotFound, data)
			}
			// Release antes do Hold (empty rollback): o Hold tardio nunca lança.
			rel, err := tx.GetCommand(ctx, c.Reference, domain.CmdRelease)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if rel != nil && rel.Outcome == domain.OutcomeNoop {
				if err := tx.InsertHold(ctx, domain.Hold{ID: newID(), ReferenceID: c.Reference, AccountID: c.AccountID,
					Amount: c.Amount, Currency: c.Currency, Status: domain.HoldCancelled}); err != nil {
					return domain.CommandRecord{}, err
				}
				return failure(domain.ReasonCancelled, data)
			}
			switch {
			case cust.Type != domain.AccountCustomer || cust.Status != domain.StatusActive:
				return failure(domain.ReasonAccountNotActive, data)
			case cust.Currency != c.Currency:
				return failure(domain.ReasonCurrencyMismatch, data)
			}
			if _, ok := cust.AvailableFor(domain.Debit, c.Amount); !ok {
				return failure(domain.ReasonInsufficientFunds, data)
			}
			jid, err := s.post(ctx, tx, domain.JournalHold, c.Reference, c.Currency, nil,
				domain.Leg{AccountID: cust.ID, Direction: domain.Debit, Amount: c.Amount},
				domain.Leg{AccountID: transit.ID, Direction: domain.Credit, Amount: c.Amount})
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if err := tx.InsertHold(ctx, domain.Hold{ID: newID(), ReferenceID: c.Reference, AccountID: c.AccountID,
				JournalID: &jid, Amount: c.Amount, Currency: c.Currency, Status: domain.HoldHeld}); err != nil {
				return domain.CommandRecord{}, err
			}
			return success(jid, data)
		})
}

// Settle (Confirm): contabilidade trânsito → reserva; sem efeito no disponível do cliente.
func (s *Service) Settle(ctx context.Context, c SettleCmd) (Result, error) {
	if err := domain.ValidateReference(c.Reference); err != nil {
		return Result{}, err
	}
	return s.execute(ctx, domain.CmdSettle, c.Reference, hashOf("SETTLE", c.Reference),
		func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error) {
			h, err := tx.GetHold(ctx, c.Reference)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if h == nil {
				return failure(domain.ReasonHoldNotFound, nil)
			}
			data := map[string]any{"account_id": h.AccountID.String(), "amount": h.Amount, "currency": h.Currency}
			if h.Status != domain.HoldHeld {
				return failure(domain.ReasonHoldNotHeld, data)
			}
			transit, err := tx.SystemAccount(ctx, domain.AccountSettlementTransit, h.Currency)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			reserve, err := tx.SystemAccount(ctx, domain.AccountReserve, h.Currency)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if _, err := tx.LockAccounts(ctx, transit.ID, reserve.ID); err != nil {
				return domain.CommandRecord{}, err
			}
			jid, err := s.post(ctx, tx, domain.JournalSettle, c.Reference, h.Currency, nil,
				domain.Leg{AccountID: transit.ID, Direction: domain.Debit, Amount: h.Amount},
				domain.Leg{AccountID: reserve.ID, Direction: domain.Credit, Amount: h.Amount})
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if err := tx.SetHoldStatus(ctx, h.ID, domain.HoldSettled); err != nil {
				return domain.CommandRecord{}, err
			}
			return success(jid, data)
		})
}

// Release (Cancel): estorno trânsito → cliente. Sem Hold prévio grava NOOP e suspende o Hold tardio.
func (s *Service) Release(ctx context.Context, c ReleaseCmd) (Result, error) {
	if err := domain.ValidateReference(c.Reference); err != nil {
		return Result{}, err
	}
	return s.execute(ctx, domain.CmdRelease, c.Reference, hashOf("RELEASE", c.Reference),
		func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error) {
			h, err := tx.GetHold(ctx, c.Reference)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if h == nil {
				return domain.CommandRecord{Outcome: domain.OutcomeNoop,
					Event: domain.Event{Data: map[string]any{"noop": true}}}, nil
			}
			data := map[string]any{"account_id": h.AccountID.String(), "amount": h.Amount, "currency": h.Currency}
			if h.Status != domain.HoldHeld {
				return failure(domain.ReasonHoldNotHeld, data)
			}
			transit, err := tx.SystemAccount(ctx, domain.AccountSettlementTransit, h.Currency)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if _, err := tx.LockAccounts(ctx, transit.ID, h.AccountID); err != nil {
				return domain.CommandRecord{}, err
			}
			jid, err := s.post(ctx, tx, domain.JournalReversal, c.Reference, h.Currency, nil,
				domain.Leg{AccountID: transit.ID, Direction: domain.Debit, Amount: h.Amount},
				domain.Leg{AccountID: h.AccountID, Direction: domain.Credit, Amount: h.Amount})
			if err != nil {
				return domain.CommandRecord{}, err
			}
			if err := tx.SetHoldStatus(ctx, h.ID, domain.HoldReleased); err != nil {
				return domain.CommandRecord{}, err
			}
			return success(jid, data)
		})
}

// Transfer move valor entre duas contas de cliente em um único journal.
func (s *Service) Transfer(ctx context.Context, c TransferCmd) (Result, error) {
	if err := firstErr(domain.ValidateReference(c.Reference), domain.ValidateAmount(c.Amount),
		s.checkCurrency(c.Currency), domain.ValidateMetadata(c.Metadata)); err != nil {
		return Result{}, err
	}
	if c.From == c.To {
		return Result{}, fmt.Errorf("%w: from and to must differ", domain.ErrInvalidArgument)
	}
	data := map[string]any{"from_account_id": c.From.String(), "to_account_id": c.To.String(), "amount": c.Amount, "currency": c.Currency}
	return s.execute(ctx, domain.CmdTransfer, c.Reference, hashOf("TRANSFER", c.From, c.To, c.Amount, c.Currency, c.Metadata),
		func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error) {
			accts, err := tx.LockAccounts(ctx, c.From, c.To)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			from, okF := accts[c.From]
			to, okT := accts[c.To]
			switch {
			case !okF || !okT:
				return failure(domain.ReasonAccountNotFound, data)
			case from.Type != domain.AccountCustomer || to.Type != domain.AccountCustomer ||
				from.Status != domain.StatusActive || to.Status != domain.StatusActive:
				return failure(domain.ReasonAccountNotActive, data)
			case from.Currency != c.Currency || to.Currency != c.Currency:
				return failure(domain.ReasonCurrencyMismatch, data)
			}
			if _, ok := from.AvailableFor(domain.Debit, c.Amount); !ok {
				return failure(domain.ReasonInsufficientFunds, data)
			}
			jid, err := s.post(ctx, tx, domain.JournalTransfer, c.Reference, c.Currency, c.Metadata,
				domain.Leg{AccountID: from.ID, Direction: domain.Debit, Amount: c.Amount},
				domain.Leg{AccountID: to.ID, Direction: domain.Credit, Amount: c.Amount})
			if err != nil {
				return domain.CommandRecord{}, err
			}
			return success(jid, data)
		})
}

// Credit é o crédito externo síncrono (reserva → cliente), com origem em metadata opaco.
func (s *Service) Credit(ctx context.Context, c CreditCmd) (Result, error) {
	if err := firstErr(domain.ValidateReference(c.Reference), domain.ValidateAmount(c.Amount),
		s.checkCurrency(c.Currency), domain.ValidateMetadata(c.Metadata)); err != nil {
		return Result{}, err
	}
	data := map[string]any{"account_id": c.AccountID.String(), "amount": c.Amount, "currency": c.Currency}
	return s.execute(ctx, domain.CmdCredit, c.Reference, hashOf("CREDIT", c.AccountID, c.Amount, c.Currency, c.Metadata),
		func(ctx context.Context, tx ports.Tx) (domain.CommandRecord, error) {
			reserve, err := tx.SystemAccount(ctx, domain.AccountReserve, c.Currency)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			accts, err := tx.LockAccounts(ctx, c.AccountID, reserve.ID)
			if err != nil {
				return domain.CommandRecord{}, err
			}
			cust, found := accts[c.AccountID]
			switch {
			case !found:
				return failure(domain.ReasonAccountNotFound, data)
			case cust.Type != domain.AccountCustomer || cust.Status != domain.StatusActive:
				return failure(domain.ReasonAccountNotActive, data)
			case cust.Currency != c.Currency:
				return failure(domain.ReasonCurrencyMismatch, data)
			}
			jid, err := s.post(ctx, tx, domain.JournalExtCredit, c.Reference, c.Currency, c.Metadata,
				domain.Leg{AccountID: reserve.ID, Direction: domain.Debit, Amount: c.Amount},
				domain.Leg{AccountID: cust.ID, Direction: domain.Credit, Amount: c.Amount})
			if err != nil {
				return domain.CommandRecord{}, err
			}
			return success(jid, data)
		})
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// EnsureSystemAccounts provisiona trânsito e reserva por moeda (idempotente).
func (s *Service) EnsureSystemAccounts(ctx context.Context) error {
	cs := make([]string, 0, len(s.currencies))
	for c := range s.currencies {
		cs = append(cs, c)
	}
	sort.Strings(cs)
	for _, c := range cs {
		for _, t := range []domain.AccountType{domain.AccountSettlementTransit, domain.AccountReserve} {
			_, err := s.store.CreateAccount(ctx, domain.Account{
				ID: newID(), ExternalRef: domain.SystemRef(t, c), Type: t, Currency: c,
				AllowNegative: true, Status: domain.StatusActive,
			})
			if err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
		}
	}
	return nil
}

func (s *Service) CreateAccount(ctx context.Context, externalRef string, t domain.AccountType, currency string) (domain.Account, error) {
	if err := firstErr(domain.ValidateExternalRef(externalRef), s.checkCurrency(currency)); err != nil {
		return domain.Account{}, err
	}
	if t != domain.AccountCustomer {
		return domain.Account{}, fmt.Errorf("%w: only CUSTOMER accounts can be created through the API", domain.ErrInvalidArgument)
	}
	if len(externalRef) >= 7 && externalRef[:7] == "system:" {
		return domain.Account{}, fmt.Errorf("%w: reserved external_ref prefix", domain.ErrInvalidArgument)
	}
	same := func(a domain.Account) (domain.Account, error) {
		if a.Type != t || a.Currency != currency {
			return domain.Account{}, fmt.Errorf("%w: external_ref already used with different attributes", domain.ErrConflict)
		}
		return a, nil
	}
	if a, err := s.store.GetAccountByRef(ctx, externalRef); err == nil {
		return same(a)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Account{}, err
	}
	a, err := s.store.CreateAccount(ctx, domain.Account{ID: newID(), ExternalRef: externalRef, Type: t, Currency: currency, Status: domain.StatusActive})
	if errors.Is(err, domain.ErrConflict) {
		existing, gerr := s.store.GetAccountByRef(ctx, externalRef)
		if gerr != nil {
			return domain.Account{}, gerr
		}
		return same(existing)
	}
	return a, err
}

func (s *Service) GetAccount(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return s.store.GetAccountByID(ctx, id)
}

func (s *Service) GetAccountByRef(ctx context.Context, ref string) (domain.Account, error) {
	if err := domain.ValidateExternalRef(ref); err != nil {
		return domain.Account{}, err
	}
	return s.store.GetAccountByRef(ctx, ref)
}

func (s *Service) SetAccountStatus(ctx context.Context, id uuid.UUID, st domain.AccountStatus) (domain.Account, error) {
	if !st.Valid() {
		return domain.Account{}, fmt.Errorf("%w: bad status", domain.ErrInvalidArgument)
	}
	acc, err := s.store.GetAccountByID(ctx, id)
	if err != nil {
		return domain.Account{}, err
	}
	if acc.Type != domain.AccountCustomer {
		return domain.Account{}, fmt.Errorf("%w: system accounts are immutable", domain.ErrInvalidArgument)
	}
	if st == domain.StatusClosed {
		b, err := s.store.GetBalance(ctx, id)
		if err != nil {
			return domain.Account{}, err
		}
		if b.Ledger() != 0 {
			return domain.Account{}, fmt.Errorf("%w: account has funds or open holds", domain.ErrConflict)
		}
	}
	return s.store.SetAccountStatus(ctx, id, st)
}

func (s *Service) GetBalance(ctx context.Context, id uuid.UUID) (domain.Balance, error) {
	return s.store.GetBalance(ctx, id)
}

const (
	defaultPage = 50
	maxPage     = 200
)

func (s *Service) ListEntries(ctx context.Context, id uuid.UUID, afterSeq int64, pageSize int) ([]domain.Entry, int64, error) {
	if afterSeq < 0 || pageSize < 0 {
		return nil, 0, fmt.Errorf("%w: negative cursor or page size", domain.ErrInvalidArgument)
	}
	if pageSize == 0 {
		pageSize = defaultPage
	}
	if pageSize > maxPage {
		pageSize = maxPage
	}
	es, err := s.store.ListEntries(ctx, id, afterSeq, pageSize+1)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(es) > pageSize {
		es = es[:pageSize]
		next = es[len(es)-1].Seq
	}
	return es, next, nil
}
