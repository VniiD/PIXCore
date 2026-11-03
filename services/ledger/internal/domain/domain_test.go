package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestJournalValidate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ok := Journal{Legs: []Leg{{a, Debit, 10}, {b, Credit, 10}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, j := range map[string]Journal{
		"unbalanced": {Legs: []Leg{{a, Debit, 10}, {b, Credit, 9}}},
		"single":     {Legs: []Leg{{a, Debit, 10}}},
		"zero":       {Legs: []Leg{{a, Debit, 0}, {b, Credit, 0}}},
		"negative":   {Legs: []Leg{{a, Debit, -5}, {b, Credit, -5}}},
	} {
		if err := j.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAvailableFor(t *testing.T) {
	c := Account{Type: AccountCustomer, Balance: 100}
	if _, ok := c.AvailableFor(Debit, 100); !ok {
		t.Error("exact debit should be ok")
	}
	if _, ok := c.AvailableFor(Debit, 101); ok {
		t.Error("overdraft must fail")
	}
	if nb, _ := c.AvailableFor(Credit, 5); nb != 105 {
		t.Error("credit increases customer")
	}
	r := Account{Type: AccountReserve, Balance: 0, AllowNegative: true}
	if nb, ok := r.AvailableFor(Credit, 10); !ok || nb != -10 {
		t.Error("reserve credit decreases asset")
	}
}

func TestValidators(t *testing.T) {
	if ValidateAmount(MaxAmount+1) == nil || ValidateAmount(0) == nil || ValidateAmount(1) != nil {
		t.Error("amount")
	}
	if ValidateCurrency("brl") == nil || ValidateCurrency("BRL") != nil {
		t.Error("currency")
	}
	if ValidateReference("a b") == nil || ValidateReference("E123:abc-1") != nil {
		t.Error("reference")
	}
}
