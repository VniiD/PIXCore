// Package ledger é o SDK Go do ledger-service do PixCore (gRPC com mTLS).
package ledger

import (
	"context"
	"crypto/tls"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/pixcore/pixcore/api/gen/pixcore/ledger/v1"
	"github.com/pixcore/pixcore/libs/platform/tlsconf"
)

type Client struct {
	conn *grpc.ClientConn
	api  ledgerv1.LedgerClient
	// Timeout aplicado a cada chamada sem deadline próprio.
	Timeout time.Duration
}

type Options struct {
	Addr       string
	CertFile   string // certificado de cliente (mTLS)
	KeyFile    string
	CAFile     string // CA que assina o servidor
	ServerName string
}

func Dial(o Options) (*Client, error) {
	if o.CertFile == "" || o.KeyFile == "" {
		return nil, errors.New("client certificate is required (mTLS)")
	}
	cfg, err := tlsconf.Client(o.CertFile, o.KeyFile, o.CAFile, o.ServerName)
	if err != nil {
		return nil, err
	}
	return DialTLS(o.Addr, cfg)
}

func DialTLS(addr string, cfg *tls.Config) (*Client, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, err
	}
	return &Client{conn: cc, api: ledgerv1.NewLedgerClient(cc), Timeout: 10 * time.Second}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// API devolve o cliente gRPC gerado para chamadas não cobertas pelos atalhos.
func (c *Client) API() ledgerv1.LedgerClient { return c.api }

func (c *Client) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.Timeout)
}

func (c *Client) CreateAccount(ctx context.Context, externalRef, currency string) (*ledgerv1.Account, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.api.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{ExternalRef: externalRef, Currency: currency, Type: ledgerv1.AccountType_ACCOUNT_TYPE_CUSTOMER})
}

// Credit é idempotente por referenceID; repetir após timeout é seguro.
func (c *Client) Credit(ctx context.Context, referenceID, accountID string, amount int64, currency string, metadata map[string]string) (*ledgerv1.CreditResponse, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.api.Credit(ctx, &ledgerv1.CreditRequest{ReferenceId: referenceID, AccountId: accountID, Amount: amount, Currency: currency, Metadata: metadata})
}

func (c *Client) GetBalance(ctx context.Context, accountID string) (*ledgerv1.Balance, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.api.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: accountID})
}

func (c *Client) ListEntries(ctx context.Context, accountID string, afterSeq int64, pageSize int32) (*ledgerv1.EntryPage, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.api.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: accountID, AfterSeq: afterSeq, PageSize: pageSize})
}

// IsRetryable indica falhas transitórias em que repetir a mesma chamada idempotente é seguro.
func IsRetryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return false
}

// RejectReason extrai a razão de negócio (ex.: ACCOUNT_NOT_ACTIVE) de um erro de Credit.
func RejectReason(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if ei, ok := d.(interface{ GetReason() string }); ok {
			return ei.GetReason()
		}
	}
	return ""
}
