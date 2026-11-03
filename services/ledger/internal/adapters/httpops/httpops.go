// Package httpops expõe health checks e métricas em porta operacional (não pública).
package httpops

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry        *prometheus.Registry
	Violations      *prometheus.GaugeVec
	StaleHolds      prometheus.Gauge
	OutboxPending   prometheus.Gauge
	OutboxOldestAge prometheus.Gauge
	LastVerifyUnix  prometheus.Gauge
	DLQMessages     prometheus.Counter
}

func NewMetrics() *Metrics {
	m := &Metrics{
		Registry:        prometheus.NewRegistry(),
		Violations:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "ledger_invariant_violations", Help: "Violações das invariantes do ledger (qualquer valor > 0 é crítico)."}, []string{"invariant"}),
		StaleHolds:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "ledger_stale_holds", Help: "Holds abertos além da idade máxima."}),
		OutboxPending:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "ledger_outbox_pending", Help: "Mensagens do outbox ainda não publicadas."}),
		OutboxOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{Name: "ledger_outbox_oldest_age_seconds", Help: "Idade da mensagem pendente mais antiga."}),
		LastVerifyUnix:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "ledger_last_verify_timestamp_seconds", Help: "Última verificação de invariantes concluída."}),
		DLQMessages:     prometheus.NewCounter(prometheus.CounterOpts{Name: "ledger_dlq_messages_total", Help: "Mensagens enviadas à DLQ."}),
	}
	m.Registry.MustRegister(m.Violations, m.StaleHolds, m.OutboxPending, m.OutboxOldestAge, m.LastVerifyUnix, m.DLQMessages)
	return m
}

type Checker func(ctx context.Context) error

func NewServer(addr string, m *Metrics, ready Checker) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 14}
}
