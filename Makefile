.PHONY: build test test-integration proto vet dev-secrets up down demo infra-test-up infra-test-down

GO ?= go
PG_DSN ?= postgres://ledger_owner:owner@127.0.0.1:55432/ledger?sslmode=disable
KAFKA_BROKERS ?= 127.0.0.1:59092

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/ledger-service ./services/ledger/cmd/ledger-service
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/ledgerctl ./tools/ledgerctl

vet:
	$(GO) vet ./...

# Testes unitários (integrações são puladas sem PIXCORE_TEST_DATABASE_URL).
test:
	$(GO) test -race ./...

# Sobe PostgreSQL e Kafka descartáveis e roda TODOS os testes, inclusive integração.
test-integration: infra-test-up
	PIXCORE_TEST_DATABASE_URL='$(PG_DSN)' PIXCORE_TEST_KAFKA_BROKERS=$(KAFKA_BROKERS) $(GO) test -race -count=1 ./...

infra-test-up:
	@docker rm -f pixcore-test-pg pixcore-test-kafka >/dev/null 2>&1 || true
	docker run -d --name pixcore-test-pg -e POSTGRES_PASSWORD=owner -e POSTGRES_USER=ledger_owner -e POSTGRES_DB=ledger \
		-p 127.0.0.1:55432:5432 --tmpfs /var/lib/postgresql/data postgres:17-alpine -c fsync=off >/dev/null
	docker run -d --name pixcore-test-kafka -p 127.0.0.1:59092:9092 \
		-e KAFKA_NODE_ID=1 -e KAFKA_PROCESS_ROLES=broker,controller \
		-e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://127.0.0.1:59092 \
		-e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
		-e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
		-e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
		-e KAFKA_AUTO_CREATE_TOPICS_ENABLE=false apache/kafka:3.9.0 >/dev/null
	@echo "aguardando infraestrutura..."; sleep 15

infra-test-down:
	-docker rm -f pixcore-test-pg pixcore-test-kafka

proto:
	protoc -I api/proto --go_out=api/gen --go_opt=paths=source_relative \
		--go-grpc_out=api/gen --go-grpc_opt=paths=source_relative api/proto/pixcore/ledger/v1/ledger.proto

dev-secrets:
	./scripts/gen-dev-certs.sh

up: dev-secrets
	docker compose -f deploy/docker-compose.yml up -d --build

down:
	docker compose -f deploy/docker-compose.yml down -v

# Demonstração: cria conta, credita e consulta saldo via gRPC com mTLS (requer `make up`).
demo: build
	@export LEDGER_ADDR=127.0.0.1:9443 LEDGER_SERVER_NAME=localhost LEDGER_CA_FILE=deploy/.dev/certs/ca.crt \
		LEDGER_CERT_FILE=deploy/.dev/certs/admin.crt LEDGER_KEY_FILE=deploy/.dev/certs/admin.key; \
	id=$$(./bin/ledgerctl create-account demo-$$(date +%s) BRL | sed -n 's/.*"id": "\(.*\)".*/\1/p'); \
	./bin/ledgerctl credit demo-credit-$$id $$id 12345 BRL >/dev/null && ./bin/ledgerctl balance $$id
