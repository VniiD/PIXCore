package config

import "testing"

func TestValidateProductionRules(t *testing.T) {
	base := Config{Env: "production", Currencies: []string{"BRL"},
		DatabaseURL: "postgres://u@h/db?sslmode=verify-full", MigrateURL: "postgres://u@h/db?sslmode=verify-full",
		TLSCert: "c", TLSKey: "k", TLSClientCA: "ca", ACLFile: "acl", RequireSignature: true}
	if err := base.Validate(true); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.DatabaseURL = "postgres://u@h/db?sslmode=disable"
	if bad.Validate(true) == nil {
		t.Error("insecure DB accepted in production")
	}
	bad = base
	bad.TLSClientCA = ""
	if bad.Validate(true) == nil {
		t.Error("missing mTLS accepted")
	}
	bad = base
	bad.KafkaBrokers = []string{"b:9092"}
	if bad.Validate(true) == nil {
		t.Error("unsigned plaintext kafka accepted in production")
	}
	dev := bad
	dev.Env = "development"
	dev.DatabaseURL = "postgres://u@h/db?sslmode=disable"
	if err := dev.Validate(true); err != nil {
		t.Errorf("development should relax: %v", err)
	}
}
