package grpc_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/pixcore/pixcore/api/gen/pixcore/ledger/v1"
	ledgergrpc "github.com/pixcore/pixcore/services/ledger/internal/adapters/grpc"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
	"github.com/pixcore/pixcore/services/ledger/internal/testutil"
)

func TestMTLSAndACL(t *testing.T) {
	store, _ := testutil.NewStore(t)
	svc, _ := app.NewService(store, nil, app.Config{Currencies: []string{"BRL"}})
	ctx := context.Background()
	if err := svc.EnsureSystemAccounts(ctx); err != nil {
		t.Fatal(err)
	}

	ca, rogueCA := testutil.NewCA(t), testutil.NewCA(t)
	srvCert := ca.Issue(t, "ledger", true)
	gs, err := ledgergrpc.NewServer(svc, ledgergrpc.Options{
		TLS: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{srvCert},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.Pool},
		ACL: ledgergrpc.ACL{
			"payment-service": {"Credit", "GetBalance"},
			"admin":           {"*"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	dial := func(cert *tls.Certificate, trust *testutil.CA) ledgerv1.LedgerClient {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool, ServerName: "localhost"}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		return ledgerv1.NewLedgerClient(cc)
	}
	issue := func(c *testutil.CA, n string) *tls.Certificate { x := c.Issue(t, n, false); return &x }
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	admin := dial(issue(ca, "admin"), ca)
	acc, err := admin.CreateAccount(callCtx, &ledgerv1.CreateAccountRequest{ExternalRef: "acct-1", Type: ledgerv1.AccountType_ACCOUNT_TYPE_CUSTOMER, Currency: "BRL"})
	if err != nil {
		t.Fatal(err)
	}

	pay := dial(issue(ca, "payment-service"), ca)
	req := &ledgerv1.CreditRequest{ReferenceId: "E2E1", AccountId: acc.Id, Amount: 1500, Currency: "BRL", Metadata: map[string]string{"origin": "spi"}}
	r1, err := pay.Credit(callCtx, req)
	if err != nil || r1.Replayed {
		t.Fatalf("credit: %v %+v", err, r1)
	}
	if r2, err := pay.Credit(callCtx, req); err != nil || !r2.Replayed || r2.JournalId != r1.JournalId {
		t.Fatalf("replay: %v %+v", err, r2)
	}
	bal, err := pay.GetBalance(callCtx, &ledgerv1.GetBalanceRequest{AccountId: acc.Id})
	if err != nil || bal.Available != 1500 {
		t.Fatalf("balance: %v %+v", err, bal)
	}
	req.Amount = 1
	if _, err := pay.Credit(callCtx, req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("reused reference: %v", err)
	}
	if _, err := pay.Credit(callCtx, &ledgerv1.CreditRequest{ReferenceId: "E2E2", AccountId: "00000000-0000-0000-0000-000000000001", Amount: 1, Currency: "BRL"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := pay.Credit(callCtx, &ledgerv1.CreditRequest{ReferenceId: "E2E3", AccountId: acc.Id, Amount: -5, Currency: "BRL"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("negative amount: %v", err)
	}
	// Método fora da ACL.
	if _, err := pay.CreateAccount(callCtx, &ledgerv1.CreateAccountRequest{ExternalRef: "x", Type: ledgerv1.AccountType_ACCOUNT_TYPE_CUSTOMER, Currency: "BRL"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("acl: %v", err)
	}
	// Identidade desconhecida.
	if _, err := dial(issue(ca, "stranger"), ca).GetBalance(callCtx, &ledgerv1.GetBalanceRequest{AccountId: acc.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stranger: %v", err)
	}
	// Sem certificado de cliente e com certificado de outra CA: handshake falha.
	if _, err := dial(nil, nil).GetBalance(callCtx, &ledgerv1.GetBalanceRequest{AccountId: acc.Id}); status.Code(err) != codes.Unavailable {
		t.Fatalf("no client cert should be rejected: %v", err)
	}
	if _, err := dial(issue(rogueCA, "admin"), nil).GetBalance(callCtx, &ledgerv1.GetBalanceRequest{AccountId: acc.Id}); status.Code(err) != codes.Unavailable {
		t.Fatalf("rogue CA should be rejected: %v", err)
	}
}
