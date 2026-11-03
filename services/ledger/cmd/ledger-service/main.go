// Comando ledger-service: ledger de partidas dobradas do PixCore.
//
//	ledger-service serve        (padrão) inicia gRPC, consumidor, relay e jobs
//	ledger-service migrate      aplica migrações com o papel dono do schema
//	ledger-service healthcheck  sonda /livez (para HEALTHCHECK em imagem distroless)
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixcore/pixcore/libs/platform/events"
	"github.com/pixcore/pixcore/libs/platform/logging"
	"github.com/pixcore/pixcore/libs/platform/tlsconf"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/grpc"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/httpops"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/kafka"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/postgres"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
	"github.com/pixcore/pixcore/services/ledger/internal/config"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	log := logging.New("ledger-service", cfg.LogLevel)
	slog.SetDefault(log)

	switch cmd {
	case "migrate":
		err = runMigrate(cfg)
	case "healthcheck":
		err = healthcheck(cfg)
	case "serve":
		err = runServe(cfg, log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}

func runMigrate(cfg config.Config) error {
	if err := cfg.Validate(false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, cfg.MigrateURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return postgres.Migrate(ctx, conn)
}

func healthcheck(cfg config.Config) error {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + cfg.OpsAddr + "/livez")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func kafkaTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.KafkaTLSCA == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(cfg.KafkaTLSCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("LEDGER_KAFKA_TLS_CA_FILE has no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}

func runServe(cfg config.Config, log *slog.Logger) error {
	if err := cfg.Validate(true); err != nil {
		return err
	}
	if cfg.Env != "production" {
		log.Warn("running in development mode: relaxed transport checks")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database url: %w", err)
	}
	pcfg.ConnConfig.RuntimeParams["application_name"] = "ledger-service"
	pcfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := postgres.NewStore(pool)
	if err := store.CheckSchema(ctx); err != nil {
		return err
	}

	keyring, err := events.NewKeyring(cfg.HMACSigningID, cfg.HMACKeys)
	if err != nil {
		return err
	}
	svc, err := app.NewService(store, keyring, app.Config{Currencies: cfg.Currencies})
	if err != nil {
		return err
	}
	if err := svc.EnsureSystemAccounts(ctx); err != nil {
		return fmt.Errorf("system accounts: %w", err)
	}

	metrics := httpops.NewMetrics()
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	var pub *kafka.Publisher
	ready := func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			return err
		}
		if pub != nil {
			return pub.Ping(ctx)
		}
		return nil
	}

	if len(cfg.KafkaBrokers) > 0 {
		kt, err := kafkaTLS(cfg)
		if err != nil {
			return err
		}
		kc := kafka.Config{Brokers: cfg.KafkaBrokers, TLS: kt, SASLUser: cfg.KafkaSASLUser, SASLPassword: cfg.KafkaSASLPassword,
			ConsumerGroup: cfg.KafkaGroup, DLQTopic: cfg.KafkaDLQTopic, RequireSignature: cfg.RequireSignature,
			OnDeadLetter: metrics.DLQMessages.Inc}
		if pub, err = kafka.NewPublisher(kc); err != nil {
			return err
		}
		defer pub.Close()
		cons, err := kafka.NewConsumer(kc, svc, keyring, log)
		if err != nil {
			return err
		}
		defer cons.Close()
		run(func() { kafka.RunRelay(ctx, store, pub, 200*time.Millisecond, log) })
		run(func() { cons.Run(ctx) })
	} else {
		log.Warn("messaging disabled (LEDGER_KAFKA_BROKERS empty): outbox will accumulate")
	}

	run(func() { runJobs(ctx, cfg, store, metrics, log) })

	serverTLS, err := tlsconf.Server(cfg.TLSCert, cfg.TLSKey, cfg.TLSClientCA)
	if err != nil {
		return err
	}
	acl, err := grpc.LoadACL(cfg.ACLFile)
	if err != nil {
		return err
	}
	gs, err := grpc.NewServer(svc, grpc.Options{TLS: serverTLS, ACL: acl, RatePerSecond: cfg.RatePerSecond, RateBurst: cfg.RateBurst, Logger: log})
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	ops := httpops.NewServer(cfg.OpsAddr, metrics, ready)

	errc := make(chan error, 2)
	go func() { errc <- gs.Serve(lis) }()
	go func() {
		if err := ops.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Info("ledger-service started", "grpc", cfg.GRPCAddr, "ops", cfg.OpsAddr)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-errc:
		log.Error("server failed", "err", err.Error())
	}
	stop()
	done := make(chan struct{})
	go func() { gs.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		gs.Stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ops.Shutdown(shutdown)
	wg.Wait()
	return err
}

func runJobs(ctx context.Context, cfg config.Config, store *postgres.Store, m *httpops.Metrics, log *slog.Logger) {
	t := time.NewTicker(cfg.VerifyInterval)
	defer t.Stop()
	var lastSnapshot, lastPurge time.Time
	for {
		if r, err := store.Verify(ctx, cfg.MaxHoldAge); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("invariant verification failed to run", "err", err.Error())
		} else {
			m.Violations.WithLabelValues("unbalanced_journal").Set(float64(r.UnbalancedJournals))
			m.Violations.WithLabelValues("balance_mismatch").Set(float64(r.BalanceMismatches))
			m.Violations.WithLabelValues("currency_imbalance").Set(float64(r.CurrencyImbalances))
			m.StaleHolds.Set(float64(r.StaleHolds))
			m.LastVerifyUnix.SetToCurrentTime()
			if r.Violations() > 0 {
				log.Error("LEDGER INVARIANT VIOLATED", "unbalanced_journals", r.UnbalancedJournals,
					"balance_mismatches", r.BalanceMismatches, "currency_imbalances", r.CurrencyImbalances)
			}
		}
		if pending, age, err := store.OutboxLag(ctx); err == nil {
			m.OutboxPending.Set(float64(pending))
			m.OutboxOldestAge.Set(age.Seconds())
		}
		now := time.Now().UTC()
		if now.Truncate(24 * time.Hour).After(lastSnapshot) {
			if _, err := store.Snapshot(ctx, now.AddDate(0, 0, -1)); err != nil {
				log.Error("snapshot failed", "err", err.Error())
			} else {
				lastSnapshot = now.Truncate(24 * time.Hour)
			}
		}
		if now.Sub(lastPurge) > time.Hour {
			if err := store.Purge(ctx, cfg.RetentionAge); err != nil {
				log.Error("purge failed", "err", err.Error())
			} else {
				lastPurge = now
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
