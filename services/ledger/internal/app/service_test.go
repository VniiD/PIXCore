package app_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pixcore/pixcore/services/ledger/internal/adapters/postgres"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
	"github.com/pixcore/pixcore/services/ledger/internal/domain"
	"github.com/pixcore/pixcore/services/ledger/internal/testutil"
)

type env struct {
	t     *testing.T
	svc   *app.Service
	store *postgres.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	store, _ := testutil.NewStore(t)
	svc, err := app.NewService(store, nil, app.Config{Currencies: []string{"BRL", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureSystemAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &env{t, svc, store}
}

func (e *env) account(name string, fund int64) uuid.UUID {
	e.t.Helper()
	a, err := e.svc.CreateAccount(context.Background(), name, domain.AccountCustomer, "BRL")
	if err != nil {
		e.t.Fatal(err)
	}
	if fund > 0 {
		if _, err := e.svc.Credit(context.Background(), app.CreditCmd{Reference: "seed-" + name, AccountID: a.ID, Amount: fund, Currency: "BRL"}); err != nil {
			e.t.Fatal(err)
		}
	}
	return a.ID
}

func (e *env) balance(id uuid.UUID) domain.Balance {
	e.t.Helper()
	b, err := e.svc.GetBalance(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) invariants() {
	e.t.Helper()
	r, err := e.store.Verify(context.Background(), time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.Violations() != 0 {
		e.t.Fatalf("invariants violated: %+v", r)
	}
}

func (e *env) outboxCount(typ string) int {
	e.t.Helper()
	var n int
	err := e.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE convert_from(payload,'UTF8')::jsonb->>'type' = $1`, typ).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

var ctx = context.Background()

func TestHoldSettleFlow(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	r, err := e.svc.Hold(ctx, app.HoldCmd{Reference: "P1", AccountID: a, Amount: 300, Currency: "BRL"})
	if err != nil || r.Outcome != domain.OutcomeSuccess {
		t.Fatalf("hold: %+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 700 || b.Held != 300 || b.Ledger() != 1000 {
		t.Fatalf("after hold: %+v", b)
	}
	if r, err = e.svc.Settle(ctx, app.SettleCmd{Reference: "P1"}); err != nil || r.Outcome != domain.OutcomeSuccess {
		t.Fatalf("settle: %+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 700 || b.Held != 0 {
		t.Fatalf("after settle: %+v", b)
	}
	// Release após SETTLED é rejeitado.
	if r, _ = e.svc.Release(ctx, app.ReleaseCmd{Reference: "P1"}); r.Outcome != domain.OutcomeFailure || r.Reason != domain.ReasonHoldNotHeld {
		t.Fatalf("release after settle: %+v", r)
	}
	if e.outboxCount("ledger.hold.succeeded") != 1 || e.outboxCount("ledger.settled") != 1 {
		t.Fatal("events missing")
	}
	e.invariants()
}

func TestHoldInsufficientFundsIsStickyAndReplayed(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 100)
	r, _ := e.svc.Hold(ctx, app.HoldCmd{Reference: "P2", AccountID: a, Amount: 500, Currency: "BRL"})
	if r.Outcome != domain.OutcomeFailure || r.Reason != domain.ReasonInsufficientFunds {
		t.Fatalf("got %+v", r)
	}
	// Saldo entra depois; a reentrega tardia do MESMO Hold nunca é reavaliada (F4).
	e.account("cust-b", 0)
	if _, err := e.svc.Credit(ctx, app.CreditCmd{Reference: "topup", AccountID: a, Amount: 10_000, Currency: "BRL"}); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Hold(ctx, app.HoldCmd{Reference: "P2", AccountID: a, Amount: 500, Currency: "BRL"})
	if err != nil || r.Outcome != domain.OutcomeFailure || !r.Replayed {
		t.Fatalf("replay: %+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 10_100 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	if n := e.outboxCount("ledger.hold.failed"); n != 2 {
		t.Fatalf("event must be re-emitted, got %d", n)
	}
	e.invariants()
}

func TestHoldReplayIdempotentAndMismatch(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	cmd := app.HoldCmd{Reference: "P3", AccountID: a, Amount: 200, Currency: "BRL"}
	if _, err := e.svc.Hold(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Hold(ctx, cmd)
	if err != nil || !r.Replayed || r.Outcome != domain.OutcomeSuccess {
		t.Fatalf("replay %+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 800 || b.Held != 200 {
		t.Fatalf("double hold: %+v", b)
	}
	cmd.Amount = 999
	if _, err := e.svc.Hold(ctx, cmd); !errors.Is(err, domain.ErrIdempotencyMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	e.invariants()
}

func TestReleaseBeforeHoldCancelsLateHold(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	r, err := e.svc.Release(ctx, app.ReleaseCmd{Reference: "P4"})
	if err != nil || r.Outcome != domain.OutcomeNoop {
		t.Fatalf("release: %+v %v", r, err)
	}
	r, err = e.svc.Hold(ctx, app.HoldCmd{Reference: "P4", AccountID: a, Amount: 100, Currency: "BRL"})
	if err != nil || r.Outcome != domain.OutcomeFailure || r.Reason != domain.ReasonCancelled {
		t.Fatalf("late hold: %+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 1000 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	e.invariants()
}

func TestReleaseRestoresFunds(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	if _, err := e.svc.Hold(ctx, app.HoldCmd{Reference: "P5", AccountID: a, Amount: 400, Currency: "BRL"}); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Release(ctx, app.ReleaseCmd{Reference: "P5"})
	if err != nil || r.Outcome != domain.OutcomeSuccess {
		t.Fatalf("%+v %v", r, err)
	}
	if b := e.balance(a); b.Available != 1000 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	if r, _ := e.svc.Settle(ctx, app.SettleCmd{Reference: "P5"}); r.Outcome != domain.OutcomeFailure {
		t.Fatalf("settle after release: %+v", r)
	}
	e.invariants()
}

func TestTransferAndCredit(t *testing.T) {
	e := setup(t)
	a, b := e.account("cust-a", 500), e.account("cust-b", 0)
	r, err := e.svc.Transfer(ctx, app.TransferCmd{Reference: "T1", From: a, To: b, Amount: 200, Currency: "BRL", Metadata: map[string]string{"k": "v"}})
	if err != nil || r.Outcome != domain.OutcomeSuccess {
		t.Fatalf("%+v %v", r, err)
	}
	if e.balance(a).Available != 300 || e.balance(b).Available != 200 {
		t.Fatal("transfer balances")
	}
	if r, _ = e.svc.Transfer(ctx, app.TransferCmd{Reference: "T2", From: a, To: b, Amount: 301, Currency: "BRL"}); r.Reason != domain.ReasonInsufficientFunds {
		t.Fatalf("%+v", r)
	}
	if _, err := e.svc.Transfer(ctx, app.TransferCmd{Reference: "T3", From: a, To: a, Amount: 1, Currency: "BRL"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("self transfer: %v", err)
	}
	// Crédito para conta inexistente / congelada.
	if r, _ = e.svc.Credit(ctx, app.CreditCmd{Reference: "C1", AccountID: uuid.New(), Amount: 1, Currency: "BRL"}); r.Reason != domain.ReasonAccountNotFound {
		t.Fatalf("%+v", r)
	}
	if _, err := e.svc.SetAccountStatus(ctx, b, domain.StatusFrozen); err != nil {
		t.Fatal(err)
	}
	if r, _ = e.svc.Credit(ctx, app.CreditCmd{Reference: "C2", AccountID: b, Amount: 1, Currency: "BRL"}); r.Reason != domain.ReasonAccountNotActive {
		t.Fatalf("%+v", r)
	}
	if _, err := e.svc.SetAccountStatus(ctx, b, domain.StatusClosed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("close with funds must fail: %v", err)
	}
	e.invariants()
}

func TestCreateAccountIdempotency(t *testing.T) {
	e := setup(t)
	a1, err := e.svc.CreateAccount(ctx, "acct-1", domain.AccountCustomer, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := e.svc.CreateAccount(ctx, "acct-1", domain.AccountCustomer, "BRL")
	if err != nil || a1.ID != a2.ID {
		t.Fatalf("not idempotent: %v", err)
	}
	if _, err := e.svc.CreateAccount(ctx, "acct-1", domain.AccountCustomer, "USD"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want conflict: %v", err)
	}
	if _, err := e.svc.CreateAccount(ctx, "system:RESERVE:BRL", domain.AccountCustomer, "BRL"); err == nil {
		t.Fatal("system prefix must be rejected")
	}
	if _, err := e.svc.CreateAccount(ctx, "x", domain.AccountReserve, "BRL"); err == nil {
		t.Fatal("non-customer type must be rejected")
	}
	if _, err := e.svc.CreateAccount(ctx, "x", domain.AccountCustomer, "EUR"); err == nil {
		t.Fatal("disabled currency must be rejected")
	}
}

func TestInboxDeduplicates(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 100)
	c := app.WithDelivery(ctx, "msg-1", "test")
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Hold(c, app.HoldCmd{Reference: "P6", AccountID: a, Amount: 10, Currency: "BRL"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.outboxCount("ledger.hold.succeeded"); n != 1 {
		t.Fatalf("inbox should suppress duplicates, events=%d", n)
	}
}

func TestListEntriesPagination(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 0)
	for i := 0; i < 5; i++ {
		if _, err := e.svc.Credit(ctx, app.CreditCmd{Reference: fmt.Sprintf("c%d", i), AccountID: a, Amount: 10, Currency: "BRL"}); err != nil {
			t.Fatal(err)
		}
	}
	var seen int
	var after int64
	for {
		es, next, err := e.svc.ListEntries(ctx, a, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(es)
		if next == 0 {
			break
		}
		after = next
	}
	if seen != 5 {
		t.Fatalf("saw %d", seen)
	}
}

func TestConcurrentHoldsNeverOverdraw(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.svc.Hold(ctx, app.HoldCmd{Reference: fmt.Sprintf("H%d", i), AccountID: a, Amount: 100, Currency: "BRL"})
			if err != nil {
				t.Error(err)
				return
			}
			if r.Outcome == domain.OutcomeSuccess {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 10 {
		t.Fatalf("exactly 10 holds must succeed, got %d", ok)
	}
	if b := e.balance(a); b.Available != 0 || b.Held != 1000 {
		t.Fatalf("%+v", b)
	}
	e.invariants()
}

func TestConcurrentDuplicateCommandsPostOnce(t *testing.T) {
	e := setup(t)
	a := e.account("cust-a", 1000)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Hold(ctx, app.HoldCmd{Reference: "DUP", AccountID: a, Amount: 100, Currency: "BRL"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b := e.balance(a); b.Available != 900 || b.Held != 100 {
		t.Fatalf("%+v", b)
	}
	e.invariants()
}

// Teste de propriedade: sequência aleatória e concorrente de operações preserva as invariantes
// (soma zero por journal, saldo = Σ entries, soma por moeda, não-negatividade).
func TestPropertyRandomOperations(t *testing.T) {
	e := setup(t)
	accts := []uuid.UUID{e.account("p-a", 5000), e.account("p-b", 5000), e.account("p-c", 0)}
	rng := rand.New(rand.NewSource(42))
	type op struct {
		kind string
		ref  string
		a, b uuid.UUID
		amt  int64
	}
	var ops []op
	for i := 0; i < 300; i++ {
		ref := fmt.Sprintf("R%d", rng.Intn(120)) // colisões de reference_id são intencionais
		o := op{ref: ref, a: accts[rng.Intn(3)], b: accts[rng.Intn(3)], amt: int64(1 + rng.Intn(2500))}
		o.kind = []string{"hold", "settle", "release", "transfer", "credit"}[rng.Intn(5)]
		ops = append(ops, o)
	}
	var wg sync.WaitGroup
	ch := make(chan op)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range ch {
				var err error
				switch o.kind {
				case "hold":
					_, err = e.svc.Hold(ctx, app.HoldCmd{Reference: o.ref, AccountID: o.a, Amount: o.amt, Currency: "BRL"})
				case "settle":
					_, err = e.svc.Settle(ctx, app.SettleCmd{Reference: o.ref})
				case "release":
					_, err = e.svc.Release(ctx, app.ReleaseCmd{Reference: o.ref})
				case "transfer":
					if o.a == o.b {
						continue
					}
					_, err = e.svc.Transfer(ctx, app.TransferCmd{Reference: o.ref, From: o.a, To: o.b, Amount: o.amt, Currency: "BRL"})
				case "credit":
					_, err = e.svc.Credit(ctx, app.CreditCmd{Reference: o.ref, AccountID: o.a, Amount: o.amt, Currency: "BRL"})
				}
				if err != nil && !errors.Is(err, domain.ErrIdempotencyMismatch) {
					t.Errorf("%s %s: %v", o.kind, o.ref, err)
				}
			}
		}()
	}
	for _, o := range ops {
		ch <- o
	}
	close(ch)
	wg.Wait()
	for _, a := range accts {
		if b := e.balance(a); b.Available < 0 || b.Held < 0 {
			t.Fatalf("negative: %+v", b)
		}
	}
	e.invariants()
}
