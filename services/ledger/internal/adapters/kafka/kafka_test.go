package kafka_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/pixcore/pixcore/libs/platform/events"
	"github.com/pixcore/pixcore/services/ledger/internal/adapters/kafka"
	"github.com/pixcore/pixcore/services/ledger/internal/app"
	"github.com/pixcore/pixcore/services/ledger/internal/domain"
	"github.com/pixcore/pixcore/services/ledger/internal/testutil"
)

// Ciclo completo: comando assinado -> consumidor -> ledger -> outbox -> relay -> evento.
func TestCommandsToEventsEndToEnd(t *testing.T) {
	brokers := os.Getenv("PIXCORE_TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("PIXCORE_TEST_KAFKA_BROKERS not set")
	}
	store, _ := testutil.NewStore(t)
	ring, err := events.NewKeyring("k1", map[string][]byte{"k1": []byte(strings.Repeat("s", 32))})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := app.NewService(store, ring, app.Config{Currencies: []string{"BRL"}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.EnsureSystemAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	acct, _ := svc.CreateAccount(ctx, "k-acct", domain.AccountCustomer, "BRL")
	if _, err := svc.Credit(ctx, app.CreditCmd{Reference: "seed", AccountID: acct.ID, Amount: 1000, Currency: "BRL"}); err != nil {
		t.Fatal(err)
	}

	// Tópicos fixos (do serviço): recria-os limpos para que a execução seja determinística.
	adm := kadm.NewClient(mustClient(t, brokers))
	topics := []string{app.TopicCommands, app.TopicEvents, "pixcore.ledger.commands.v1.dlq"}
	_, _ = adm.DeleteTopics(ctx, topics...)
	for _, topic := range topics {
		for {
			_, err := adm.CreateTopic(ctx, 1, 1, nil, topic)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("create %s: %v", topic, err)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	time.Sleep(2 * time.Second) // propagação de metadados do líder

	cfg := kafka.Config{Brokers: strings.Split(brokers, ","), ConsumerGroup: "t-" + uuid.NewString(), DLQTopic: "pixcore.ledger.commands.v1.dlq", RequireSignature: true}
	pub, err := kafka.NewPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	cons, err := kafka.NewConsumer(cfg, svc, ring, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	go kafka.RunRelay(ctx, store, pub, 50*time.Millisecond, slog.Default())
	go cons.Run(ctx)

	producer := mustClient(t, brokers)
	ref := "kref-" + uuid.NewString()[:8]
	send := func(typ string, data any, sign bool, id string) {
		env, _ := events.New(id, typ, "test", ref, data)
		payload, _ := json.Marshal(env)
		r := &kgo.Record{Topic: app.TopicCommands, Key: []byte(ref), Value: payload}
		if sign {
			r.Headers = []kgo.RecordHeader{{Key: events.SignatureHeader, Value: []byte(ring.Sign(payload))}}
		}
		if err := producer.ProduceSync(ctx, r).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}

	evClient := mustClient(t, brokers, kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{app.TopicEvents: {0: kgo.NewOffset().AtStart()}}))
	dlqClient := mustClient(t, brokers, kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{"pixcore.ledger.commands.v1.dlq": {0: kgo.NewOffset().AtStart()}}))

	hold := map[string]any{"reference_id": ref, "account_id": acct.ID, "amount": 400, "currency": "BRL"}
	send("ledger.hold.requested", hold, true, "m1")
	send("ledger.hold.requested", hold, true, "m1-retry") // reenvio: mesma ref, novo id de mensagem
	send("ledger.settle.requested", map[string]any{"reference_id": ref}, true, "m2")
	send("ledger.hold.requested", hold, false, "forged")                      // sem assinatura -> DLQ
	send("ledger.transfer.requested", map[string]any{"bogus": 1}, true, "m3") // inválido -> DLQ

	got := map[string]int{}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) && (got["ledger.hold.succeeded"] < 2 || got["ledger.settled"] < 1) {
		fs := evClient.PollFetches(ctx)
		fs.EachRecord(func(r *kgo.Record) {
			var e events.Envelope
			_ = json.Unmarshal(r.Value, &e)
			if string(r.Key) == ref {
				if err := ring.Verify(r.Value, headerOf(r, events.SignatureHeader)); err != nil {
					t.Errorf("published event has bad signature: %v", err)
				}
				got[e.Type]++
			}
		})
	}
	if got["ledger.hold.succeeded"] != 2 || got["ledger.settled"] != 1 {
		t.Fatalf("events: %v (replay must re-emit hold.succeeded)", got)
	}
	b, _ := svc.GetBalance(ctx, acct.ID)
	if b.Available != 600 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}

	dead := 0
	for dead < 2 && time.Now().Before(deadline) {
		dlqClient.PollFetches(ctx).EachRecord(func(r *kgo.Record) {
			if headerOf(r, "dlq-reason") != "" && string(r.Key) == ref {
				dead++
			}
		})
	}
	if dead != 2 {
		t.Fatalf("expected 2 DLQ messages, got %d", dead)
	}
	rep, err := store.Verify(ctx, time.Hour)
	if err != nil || rep.Violations() != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	fmt.Println("e2e ok")
}

func headerOf(r *kgo.Record, k string) string {
	for _, h := range r.Headers {
		if h.Key == k {
			return string(h.Value)
		}
	}
	return ""
}

func mustClient(t *testing.T, brokers string, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(strings.Split(brokers, ",")...)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}
