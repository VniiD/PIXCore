// Package kafka liga o outbox/inbox do ledger ao broker (publicação e consumo de comandos).
package kafka

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/pixcore/pixcore/libs/platform/events"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/postgres"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
)

type Config struct {
	Brokers       []string
	TLS           *tls.Config // nil = texto claro (apenas desenvolvimento)
	SASLUser      string
	SASLPassword  string
	ConsumerGroup string
	DLQTopic      string
	// RequireSignature rejeita comandos sem assinatura HMAC válida.
	RequireSignature bool
	// OnDeadLetter é chamado a cada mensagem enviada à DLQ (métricas).
	OnDeadLetter func()
}

func baseOpts(c Config) ([]kgo.Opt, error) {
	// Sem criação automática de tópicos: a infra provisiona os tópicos.
	opts := []kgo.Opt{kgo.SeedBrokers(c.Brokers...)}
	if c.TLS != nil {
		opts = append(opts, kgo.DialTLSConfig(c.TLS))
	}
	if c.SASLUser != "" {
		opts = append(opts, kgo.SASL(scram.Auth{User: c.SASLUser, Pass: c.SASLPassword}.AsSha512Mechanism()))
	}
	return opts, nil
}

type Publisher struct{ cl *kgo.Client }

func NewPublisher(c Config) (*Publisher, error) {
	opts, err := baseOpts(c)
	if err != nil {
		return nil, err
	}
	opts = append(opts, kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerBatchCompression(kgo.NoCompression()),
		kgo.RecordDeliveryTimeout(30*time.Second))
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &Publisher{cl}, nil
}

func (p *Publisher) Close() { p.cl.Close() }

func (p *Publisher) Ping(ctx context.Context) error { return p.cl.Ping(ctx) }

func (p *Publisher) Publish(ctx context.Context, msgs []postgres.OutboxMessage) error {
	recs := make([]*kgo.Record, len(msgs))
	for i, m := range msgs {
		r := &kgo.Record{Topic: m.Topic, Key: []byte(m.Key), Value: m.Payload}
		for k, v := range m.Headers {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
		recs[i] = r
	}
	return p.cl.ProduceSync(ctx, recs...).FirstErr()
}

// RunRelay drena o outbox até o contexto terminar.
func RunRelay(ctx context.Context, store *postgres.Store, pub *Publisher, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for {
			n, err := store.RelayBatch(ctx, 200, pub.Publish)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("outbox relay failed", "err", err.Error())
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type Consumer struct {
	cl      *kgo.Client
	dlq     *kgo.Client
	svc     *app.Service
	cfg     Config
	keyring *events.Keyring
	log     *slog.Logger
}

func NewConsumer(c Config, svc *app.Service, keyring *events.Keyring, log *slog.Logger) (*Consumer, error) {
	if c.RequireSignature && !keyring.Enabled() {
		return nil, errors.New("signature required but no HMAC keys configured")
	}
	opts, err := baseOpts(c)
	if err != nil {
		return nil, err
	}
	cl, err := kgo.NewClient(append(opts, kgo.ConsumerGroup(c.ConsumerGroup), kgo.ConsumeTopics(app.TopicCommands),
		kgo.DisableAutoCommit(), kgo.FetchMaxBytes(8<<20), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		return nil, err
	}
	dlq, err := NewPublisher(c)
	if err != nil {
		cl.Close()
		return nil, err
	}
	return &Consumer{cl: cl, dlq: dlq.cl, svc: svc, cfg: c, keyring: keyring, log: log}, nil
}

func (c *Consumer) Close() { c.cl.Close(); c.dlq.Close() }

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// handle devolve nil quando o registro pode ser confirmado (sucesso ou enviado à DLQ).
func (c *Consumer) handle(ctx context.Context, r *kgo.Record) error {
	if len(r.Value) > events.MaxEnvelopeBytes {
		return c.deadLetter(ctx, r, "envelope too large")
	}
	if c.cfg.RequireSignature || c.keyring.Enabled() {
		if err := c.keyring.Verify(r.Value, header(r, events.SignatureHeader)); err != nil {
			return c.deadLetter(ctx, r, err.Error())
		}
	}
	env, err := events.Decode(r.Value)
	if err != nil {
		return c.deadLetter(ctx, r, err.Error())
	}
	err = c.svc.HandleCommand(ctx, env)
	if errors.Is(err, app.ErrPermanent) {
		return c.deadLetter(ctx, r, err.Error())
	}
	return err
}

func (c *Consumer) deadLetter(ctx context.Context, r *kgo.Record, reason string) error {
	if c.cfg.OnDeadLetter != nil {
		c.cfg.OnDeadLetter()
	}
	c.log.Warn("message sent to DLQ", "topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "reason", reason)
	dl := &kgo.Record{Topic: c.cfg.DLQTopic, Key: r.Key, Value: r.Value, Headers: append(r.Headers,
		kgo.RecordHeader{Key: "dlq-reason", Value: []byte(reason)},
		kgo.RecordHeader{Key: "dlq-source", Value: []byte(r.Topic)})}
	return c.dlq.ProduceSync(ctx, dl).FirstErr()
}

// Run consome comandos em ordem por partição. Falhas transitórias repetem o mesmo registro com backoff.
func (c *Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		fetches := c.cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(t string, p int32, err error) {
			if ctx.Err() == nil {
				c.log.Error("fetch error", "topic", t, "partition", p, "err", err.Error())
			}
		})
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			for _, r := range p.Records {
				backoff := 100 * time.Millisecond
				for {
					err := c.handle(ctx, r)
					if err == nil {
						break
					}
					if ctx.Err() != nil {
						return
					}
					c.log.Error("command processing failed; retrying", "offset", r.Offset, "err", err.Error())
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					if backoff < 5*time.Second {
						backoff *= 2
					}
				}
				if err := c.cl.CommitRecords(ctx, r); err != nil && ctx.Err() == nil {
					c.log.Error("commit failed", "err", err.Error())
				}
			}
		})
	}
}
