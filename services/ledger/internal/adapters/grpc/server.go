// Package grpc expõe o ledger por gRPC com mTLS obrigatório e ACL por método.
package grpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/pixcore/pixcore/api/gen/pixcore/ledger/v1"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
	"github.com/pixcore/pixcore/services/ledger/internal/domain"
)

// ACL mapeia a identidade do certificado (URI SAN ou CN) para os métodos permitidos
// ("*" libera todos). Sem entrada = negado.
type ACL map[string][]string

func LoadACL(path string) (ACL, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a ACL
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("parse ACL: %w", err)
	}
	if len(a) == 0 {
		return nil, errors.New("ACL is empty: refusing to start with deny-all")
	}
	return a, nil
}

func (a ACL) allowed(identity, method string) bool {
	for _, m := range a[identity] {
		if m == "*" || m == method {
			return true
		}
	}
	return false
}

type Options struct {
	TLS            *tls.Config
	ACL            ACL
	RatePerSecond  float64 // por identidade
	RateBurst      int
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type Server struct {
	ledgerv1.UnimplementedLedgerServer
	svc  *app.Service
	opts Options

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func NewServer(svc *app.Service, o Options) (*grpc.Server, error) {
	if o.TLS == nil {
		return nil, errors.New("TLS is mandatory")
	}
	if o.ACL == nil {
		return nil, errors.New("ACL is mandatory")
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.RatePerSecond == 0 {
		o.RatePerSecond, o.RateBurst = 500, 1000
	}
	s := &Server{svc: svc, opts: o, limiters: map[string]*rate.Limiter{}}
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(o.TLS)),
		grpc.ChainUnaryInterceptor(s.recoverer, s.authz, s.timeout),
		grpc.MaxRecvMsgSize(1<<20),
		grpc.MaxSendMsgSize(4<<20),
		grpc.MaxConcurrentStreams(256),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute, MaxConnectionAge: time.Hour, MaxConnectionAgeGrace: 30 * time.Second}),
	)
	ledgerv1.RegisterLedgerServer(gs, s)
	return gs, nil
}

func identityOf(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", false
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || len(ti.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := ti.State.VerifiedChains[0][0]
	if len(leaf.URIs) > 0 {
		return leaf.URIs[0].String(), true
	}
	if leaf.Subject.CommonName != "" {
		return leaf.Subject.CommonName, true
	}
	return "", false
}

func (s *Server) limiter(id string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[id]
	if !ok {
		l = rate.NewLimiter(rate.Limit(s.opts.RatePerSecond), s.opts.RateBurst)
		s.limiters[id] = l
	}
	return l
}

func (s *Server) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.Default()
}

func (s *Server) recoverer(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log().Error("panic in handler", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return h(ctx, req)
}

func (s *Server) authz(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	id, ok := identityOf(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	method := info.FullMethod[lastSlash(info.FullMethod)+1:]
	if !s.opts.ACL.allowed(id, method) {
		s.log().Warn("access denied", "identity", id, "method", method)
		return nil, status.Error(codes.PermissionDenied, "not allowed")
	}
	if !s.limiter(id).Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	start := time.Now()
	resp, err := h(context.WithValue(ctx, identityKey{}, id), req)
	s.log().Info("rpc", "identity", id, "method", method, "code", status.Code(err).String(), "dur_ms", time.Since(start).Milliseconds())
	return resp, err
}

func (s *Server) timeout(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
	defer cancel()
	return h(ctx, req)
}

type identityKey struct{}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func (s *Server) toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, domain.ErrIdempotencyMismatch):
		return status.Error(codes.AlreadyExists, "reference_id already used with a different request")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	}
	s.log().Error("internal error", "err", err.Error())
	return status.Error(codes.Internal, "internal error")
}

func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: invalid uuid", domain.ErrInvalidArgument)
	}
	return id, nil
}

var accountTypes = map[domain.AccountType]ledgerv1.AccountType{
	domain.AccountCustomer: ledgerv1.AccountType_ACCOUNT_TYPE_CUSTOMER, domain.AccountSettlementTransit: ledgerv1.AccountType_ACCOUNT_TYPE_SETTLEMENT_TRANSIT,
	domain.AccountReserve: ledgerv1.AccountType_ACCOUNT_TYPE_RESERVE, domain.AccountFees: ledgerv1.AccountType_ACCOUNT_TYPE_FEES,
	domain.AccountInternal: ledgerv1.AccountType_ACCOUNT_TYPE_INTERNAL,
}

var statuses = map[domain.AccountStatus]ledgerv1.AccountStatus{
	domain.StatusActive: ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, domain.StatusFrozen: ledgerv1.AccountStatus_ACCOUNT_STATUS_FROZEN,
	domain.StatusClosed: ledgerv1.AccountStatus_ACCOUNT_STATUS_CLOSED,
}

func accountPB(a domain.Account) *ledgerv1.Account {
	return &ledgerv1.Account{Id: a.ID.String(), ExternalRef: a.ExternalRef, Type: accountTypes[a.Type], Currency: a.Currency,
		Status: statuses[a.Status], CreatedAt: timestamppb.New(a.CreatedAt)}
}

func (s *Server) CreateAccount(ctx context.Context, r *ledgerv1.CreateAccountRequest) (*ledgerv1.Account, error) {
	t := domain.AccountType("")
	for k, v := range accountTypes {
		if v == r.GetType() {
			t = k
		}
	}
	a, err := s.svc.CreateAccount(ctx, r.GetExternalRef(), t, r.GetCurrency())
	if err != nil {
		return nil, s.toStatus(err)
	}
	return accountPB(a), nil
}

func (s *Server) GetAccount(ctx context.Context, r *ledgerv1.GetAccountRequest) (*ledgerv1.Account, error) {
	var a domain.Account
	var err error
	switch by := r.GetBy().(type) {
	case *ledgerv1.GetAccountRequest_Id:
		var id uuid.UUID
		if id, err = parseID(by.Id); err == nil {
			a, err = s.svc.GetAccount(ctx, id)
		}
	case *ledgerv1.GetAccountRequest_ExternalRef:
		a, err = s.svc.GetAccountByRef(ctx, by.ExternalRef)
	default:
		err = fmt.Errorf("%w: id or external_ref required", domain.ErrInvalidArgument)
	}
	if err != nil {
		return nil, s.toStatus(err)
	}
	return accountPB(a), nil
}

func (s *Server) SetAccountStatus(ctx context.Context, r *ledgerv1.SetAccountStatusRequest) (*ledgerv1.Account, error) {
	id, err := parseID(r.GetId())
	if err != nil {
		return nil, s.toStatus(err)
	}
	st := domain.AccountStatus("")
	for k, v := range statuses {
		if v == r.GetStatus() {
			st = k
		}
	}
	a, err := s.svc.SetAccountStatus(ctx, id, st)
	if err != nil {
		return nil, s.toStatus(err)
	}
	return accountPB(a), nil
}

func (s *Server) Credit(ctx context.Context, r *ledgerv1.CreditRequest) (*ledgerv1.CreditResponse, error) {
	id, err := parseID(r.GetAccountId())
	if err != nil {
		return nil, s.toStatus(err)
	}
	res, err := s.svc.Credit(ctx, app.CreditCmd{Reference: r.GetReferenceId(), AccountID: id, Amount: r.GetAmount(),
		Currency: r.GetCurrency(), Metadata: r.GetMetadata()})
	if err != nil {
		return nil, s.toStatus(err)
	}
	if res.Outcome != domain.OutcomeSuccess {
		code := codes.FailedPrecondition
		if res.Reason == domain.ReasonAccountNotFound {
			code = codes.NotFound
		}
		st, derr := status.New(code, "credit rejected: "+res.Reason).WithDetails(&errdetails.ErrorInfo{Reason: res.Reason, Domain: "ledger.pixcore"})
		if derr != nil {
			return nil, status.Error(code, "credit rejected: "+res.Reason)
		}
		return nil, st.Err()
	}
	return &ledgerv1.CreditResponse{JournalId: res.JournalID.String(), Replayed: res.Replayed}, nil
}

func (s *Server) GetBalance(ctx context.Context, r *ledgerv1.GetBalanceRequest) (*ledgerv1.Balance, error) {
	id, err := parseID(r.GetAccountId())
	if err != nil {
		return nil, s.toStatus(err)
	}
	b, err := s.svc.GetBalance(ctx, id)
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.Balance{AccountId: b.AccountID.String(), Currency: b.Currency, Available: b.Available, Held: b.Held, Ledger: b.Ledger()}, nil
}

func (s *Server) ListEntries(ctx context.Context, r *ledgerv1.ListEntriesRequest) (*ledgerv1.EntryPage, error) {
	id, err := parseID(r.GetAccountId())
	if err != nil {
		return nil, s.toStatus(err)
	}
	es, next, err := s.svc.ListEntries(ctx, id, r.GetAfterSeq(), int(r.GetPageSize()))
	if err != nil {
		return nil, s.toStatus(err)
	}
	page := &ledgerv1.EntryPage{NextAfterSeq: next}
	for _, e := range es {
		dir := ledgerv1.Direction_DIRECTION_CREDIT
		if e.Direction == domain.Debit {
			dir = ledgerv1.Direction_DIRECTION_DEBIT
		}
		page.Entries = append(page.Entries, &ledgerv1.Entry{Id: e.ID.String(), JournalId: e.JournalID.String(),
			JournalKind: string(e.JournalKind), ReferenceId: e.ReferenceID, Direction: dir, Amount: e.Amount,
			Currency: e.Currency, BalanceAfter: e.BalanceAfter, Seq: e.Seq, CreatedAt: timestamppb.New(e.CreatedAt)})
	}
	return page, nil
}
