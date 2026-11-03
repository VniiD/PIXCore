// Package domain contém as regras puras do ledger: sem I/O e sem conceitos de Pix.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

type AccountType string

const (
	AccountCustomer          AccountType = "CUSTOMER"
	AccountSettlementTransit AccountType = "SETTLEMENT_TRANSIT"
	AccountReserve           AccountType = "RESERVE"
	AccountFees              AccountType = "FEES"
	AccountInternal          AccountType = "INTERNAL"
)

func (t AccountType) Valid() bool {
	switch t {
	case AccountCustomer, AccountSettlementTransit, AccountReserve, AccountFees, AccountInternal:
		return true
	}
	return false
}

// NormalSide é o lado que aumenta o saldo da conta.
func (t AccountType) NormalSide() Direction {
	switch t {
	case AccountReserve, AccountInternal:
		return Debit
	default:
		return Credit
	}
}

type AccountStatus string

const (
	StatusActive AccountStatus = "ACTIVE"
	StatusFrozen AccountStatus = "FROZEN"
	StatusClosed AccountStatus = "CLOSED"
)

func (s AccountStatus) Valid() bool {
	return s == StatusActive || s == StatusFrozen || s == StatusClosed
}

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type JournalKind string

const (
	JournalHold      JournalKind = "HOLD"
	JournalSettle    JournalKind = "SETTLEMENT"
	JournalReversal  JournalKind = "REVERSAL"
	JournalTransfer  JournalKind = "INTERNAL_TRANSFER"
	JournalExtCredit JournalKind = "EXTERNAL_CREDIT"
)

type HoldStatus string

const (
	HoldHeld      HoldStatus = "HELD"
	HoldSettled   HoldStatus = "SETTLED"
	HoldReleased  HoldStatus = "RELEASED"
	HoldCancelled HoldStatus = "CANCELLED"
)

type Command string

const (
	CmdHold     Command = "HOLD"
	CmdSettle   Command = "SETTLE"
	CmdRelease  Command = "RELEASE"
	CmdTransfer Command = "TRANSFER"
	CmdCredit   Command = "CREDIT"
)

type Outcome string

const (
	OutcomeSuccess Outcome = "SUCCESS"
	OutcomeFailure Outcome = "FAILURE"
	OutcomeNoop    Outcome = "NOOP"
)

// Razões de falha de negócio (gravadas no command_log, nunca reavaliadas).
const (
	ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"
	ReasonCancelled         = "CANCELLED"
	ReasonAccountNotFound   = "ACCOUNT_NOT_FOUND"
	ReasonAccountNotActive  = "ACCOUNT_NOT_ACTIVE"
	ReasonCurrencyMismatch  = "CURRENCY_MISMATCH"
	ReasonHoldNotFound      = "HOLD_NOT_FOUND"
	ReasonHoldNotHeld       = "HOLD_NOT_HELD"
)

var (
	ErrInvalidArgument     = errors.New("invalid argument")
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyMismatch = errors.New("reference_id reused with a different request")
	ErrConflict            = errors.New("conflict")
)

const (
	MaxAmount        int64 = 1_000_000_000_000_000 // 10^15 unidades menores
	MaxReferenceLen        = 128
	maxMetadataKeys        = 16
	maxMetadataKey         = 64
	maxMetadataValue       = 256
)

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,128}$`)
	extRefRe   = regexp.MustCompile(`^[A-Za-z0-9._:\-/]{1,128}$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, a...))
}

func ValidateCurrency(c string) error {
	if !currencyRe.MatchString(c) {
		return invalid("currency must be ISO 4217 alpha-3 uppercase")
	}
	return nil
}

func ValidateAmount(a int64) error {
	if a <= 0 || a > MaxAmount {
		return invalid("amount must be between 1 and %d", MaxAmount)
	}
	return nil
}

func ValidateReference(ref string) error {
	if !refRe.MatchString(ref) {
		return invalid("reference_id must match %s", refRe)
	}
	return nil
}

func ValidateExternalRef(ref string) error {
	if !extRefRe.MatchString(ref) {
		return invalid("external_ref must match %s", extRefRe)
	}
	return nil
}

func ValidateMetadata(m map[string]string) error {
	if len(m) > maxMetadataKeys {
		return invalid("metadata allows at most %d keys", maxMetadataKeys)
	}
	for k, v := range m {
		if k == "" || len(k) > maxMetadataKey || len(v) > maxMetadataValue {
			return invalid("metadata key/value too long or empty key")
		}
	}
	return nil
}

type Account struct {
	ID            uuid.UUID
	ExternalRef   string
	Type          AccountType
	Currency      string
	AllowNegative bool
	Status        AccountStatus
	Balance       int64
	Version       int64
	CreatedAt     time.Time
}

// CanDebit informa se o saldo comporta o débito (aplicando o lado normal da conta).
func (a Account) AvailableFor(side Direction, amount int64) (int64, bool) {
	delta := amount
	if side != a.Type.NormalSide() {
		delta = -amount
	}
	nb := a.Balance + delta
	if nb < 0 && !a.AllowNegative {
		return nb, false
	}
	return nb, true
}

type Leg struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
}

type Journal struct {
	ID          uuid.UUID
	Kind        JournalKind
	ReferenceID string
	Currency    string
	Metadata    map[string]string
	Legs        []Leg
}

// Validate garante a partida dobrada antes de qualquer escrita.
func (j Journal) Validate() error {
	if len(j.Legs) < 2 {
		return invalid("journal needs at least two entries")
	}
	var d, c int64
	for _, l := range j.Legs {
		if err := ValidateAmount(l.Amount); err != nil {
			return err
		}
		switch l.Direction {
		case Debit:
			d += l.Amount
		case Credit:
			c += l.Amount
		default:
			return invalid("bad direction")
		}
	}
	if d != c {
		return invalid("journal not balanced")
	}
	return nil
}

type Hold struct {
	ID          uuid.UUID
	ReferenceID string
	AccountID   uuid.UUID
	JournalID   *uuid.UUID
	Amount      int64
	Currency    string
	Status      HoldStatus
}

type Entry struct {
	ID           uuid.UUID
	Seq          int64
	JournalID    uuid.UUID
	JournalKind  JournalKind
	ReferenceID  string
	AccountID    uuid.UUID
	Direction    Direction
	Amount       int64
	Currency     string
	BalanceAfter int64
	CreatedAt    time.Time
}

type Balance struct {
	AccountID uuid.UUID
	Currency  string
	Available int64
	Held      int64
}

func (b Balance) Ledger() int64 { return b.Available + b.Held }

// Event é a mensagem de domínio publicada no tópico de eventos.
type Event struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// CommandRecord é o resultado gravado de um comando (idempotência lógica).
type CommandRecord struct {
	ReferenceID string
	Command     Command
	RequestHash string
	Outcome     Outcome
	Reason      string
	JournalID   *uuid.UUID
	Event       Event
}

// SystemRef devolve o external_ref estável da conta de sistema.
func SystemRef(t AccountType, currency string) string {
	return fmt.Sprintf("system:%s:%s", t, currency)
}
