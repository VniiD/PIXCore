// ledgerctl: cliente de linha de comando do ledger-service (operações e demonstração).
//
//	ledgerctl create-account <external_ref> <currency>
//	ledgerctl credit <reference_id> <account_id> <amount> <currency>
//	ledgerctl balance <account_id>
//	ledgerctl entries <account_id>
//
// Configuração: LEDGER_ADDR, LEDGER_CERT_FILE, LEDGER_KEY_FILE, LEDGER_CA_FILE, LEDGER_SERVER_NAME.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/pixcore/pixcore/sdk/go/ledger"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func need(args []string, n int, usage string) error {
	if len(args) != n {
		return fmt.Errorf("usage: ledgerctl %s", usage)
	}
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ledgerctl create-account|credit|balance|entries ...")
	}
	c, err := ledger.Dial(ledger.Options{
		Addr: os.Getenv("LEDGER_ADDR"), CertFile: os.Getenv("LEDGER_CERT_FILE"), KeyFile: os.Getenv("LEDGER_KEY_FILE"),
		CAFile: os.Getenv("LEDGER_CA_FILE"), ServerName: os.Getenv("LEDGER_SERVER_NAME"),
	})
	if err != nil {
		return err
	}
	defer c.Close()
	ctx := context.Background()
	var out any
	switch args[0] {
	case "create-account":
		if err := need(args, 3, "create-account <external_ref> <currency>"); err != nil {
			return err
		}
		out, err = c.CreateAccount(ctx, args[1], args[2])
	case "credit":
		if err := need(args, 5, "credit <reference_id> <account_id> <amount> <currency>"); err != nil {
			return err
		}
		amt, perr := strconv.ParseInt(args[3], 10, 64)
		if perr != nil {
			return perr
		}
		out, err = c.Credit(ctx, args[1], args[2], amt, args[4], nil)
	case "balance":
		if err := need(args, 2, "balance <account_id>"); err != nil {
			return err
		}
		out, err = c.GetBalance(ctx, args[1])
	case "entries":
		if err := need(args, 2, "entries <account_id>"); err != nil {
			return err
		}
		out, err = c.ListEntries(ctx, args[1], 0, 50)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}
