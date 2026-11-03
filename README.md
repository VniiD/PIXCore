# PixCore

Infraestrutura *bank-in-a-box* (Go) para pagamentos Pix, descrita em [Docs/](Docs/). Implementação por etapas do roadmap
([production-readiness](Docs/production-readiness.md)).

| Etapa | Estado |
|---|---|
| 1. `ledger-service` + testes de propriedade + SDK Go | **Implementada** (este repositório) |
| 2. `payment-service` + `spi-simulator` (Saga) | Pendente |
| 3. Reconciliação + caos | Pendente |
| 4. Adapters `provider` / `rsfn` | Pendente |
| 5. Hardening (carga, restore, runbooks, revisão externa) | Pendente |

## Sumário

1. [Arquitetura](#arquitetura)
2. [Estrutura do repositório](#estrutura-do-repositório)
3. [Uso rápido](#uso-rápido)
4. [ledger-service](#ledger-service)
5. [Modelagem](#modelagem-do-domínio)
6. [Fluxos](#fluxos)
7. [Segurança](#modelo-de-segurança)
8. [Limitações](#limitações-conhecidas)

## Arquitetura

Monorepo Go (módulo `github.com/pixcore/pixcore`), **microsserviços orientados a eventos com arquitetura hexagonal**
([ADR-0001](Docs/0001-microservices-eda-hexagonal.md)). Cada serviço é dono do seu PostgreSQL; a comunicação é gRPC (mTLS)
para consultas/crédito e Kafka (CloudEvents assinados com HMAC) para a Saga. Contexto C4 em [Docs/01-c4.md](Docs/01-c4.md).

### Arquitetura alvo

```mermaid
flowchart LR
    client([Cliente / API Gateway]) -->|gRPC mTLS| pay[payment-service]
    prov[provider-adapter] --> pay
    pay <-->|Saga| spi[spi-simulator / rsfn-adapter]
    recon[reconciliation-service] --> ledger
    recon --> pay
    pay --- pgP[(PostgreSQL payment)]
    ledger[ledger-service] --- pgL[(PostgreSQL ledger)]
    subgraph Kafka
      cmd[[ledger.commands.v1]]
      evt[[ledger.events.v1]]
      dlq[[ledger.commands.v1.dlq]]
    end
    pay --> cmd --> ledger
    ledger --> evt --> pay
    cmd -.inválida.-> dlq
```

> Estado: somente o `ledger-service` está implementado (Etapa 1); os demais componentes são alvo do roadmap.

### Arquitetura hexagonal do `ledger-service`

```mermaid
flowchart TB
    subgraph IN[Adapters de entrada]
      grpc[gRPC server<br/>mTLS + ACL + rate limit]
      kin[Kafka consumer<br/>HMAC + inbox + DLQ]
      ops[httpops<br/>/livez /readyz /metrics]
    end
    subgraph CORE[Núcleo]
      app[app.Service<br/>casos de uso + idempotência]
      dom[domain<br/>contas, lançamentos, invariantes]
      ports[[ports]]
    end
    subgraph OUT[Adapters de saída]
      pg[postgres Store<br/>triggers, outbox, verify]
      kout[Kafka publisher<br/>outbox relay]
    end
    grpc --> app
    kin --> app
    app --> dom
    app --> ports
    ports --> pg
    pg --> kout
```

## Estrutura do repositório

```
api/proto/…           contrato gRPC (fonte); api/gen/… é código gerado (make proto)
libs/platform/…       events (CloudEvents + HMAC), tlsconf (mTLS), logging (redação)
services/ledger/      ledger-service (domain, app, ports, adapters, cmd, config)
sdk/go/ledger         SDK Go (gRPC + mTLS)
tools/ledgerctl       CLI operacional / demo
deploy/               Dockerfile (distroless), docker-compose de desenvolvimento
scripts/              gen-dev-certs.sh (PKI de desenvolvimento)
Docs/                 ADRs, C4, modelos, sequências, matriz de falhas, readiness
```

## Uso rápido

```bash
make test-integration   # sobe PostgreSQL + Kafka descartáveis e roda TODOS os testes (-race)
make up && make demo    # stack completa em Docker (dev) + conta/crédito/saldo via mTLS
make down
```

`make test` roda sem infraestrutura (os testes de integração são pulados sem `PIXCORE_TEST_DATABASE_URL`).

## ledger-service

Ledger de partidas dobradas genérico ([ADR-0009](Docs/0009-ledger-design.md), [ADR-0006](Docs/0006-saga-tcc.md)).

- **gRPC (mTLS + ACL):** `CreateAccount`, `GetAccount`, `SetAccountStatus`, `Credit` (síncrono), `GetBalance`, `ListEntries`
  ([ledger.proto](api/proto/pixcore/ledger/v1/ledger.proto)).
- **Comandos assíncronos** (`pixcore.ledger.commands.v1`, CloudEvents assinados com HMAC): `ledger.hold|settle|release|transfer.requested`.
- **Eventos** (`pixcore.ledger.events.v1`) via outbox transacional: `ledger.hold.succeeded|failed`, `ledger.settled`, `ledger.released`, `ledger.transferred`, `ledger.credited` e variantes `*.failed`.
- **Idempotência lógica:** `command_log (reference_id, command)` grava sucesso, falha e `NOOP`; reentrega devolve o resultado gravado e reemite o evento; mesmo `reference_id` com corpo diferente é rejeitado.
- **Empty rollback:** `Release` antes do `Hold` grava `NOOP`; o `Hold` tardio responde `CANCELLED` sem lançar.
- **Jobs:** verificação contínua das invariantes (métricas Prometheus em `/metrics`), snapshot diário, expurgo de outbox/inbox.
- **DLQ:** mensagens inválidas, sem assinatura ou fora do contrato vão para `pixcore.ledger.commands.v1.dlq`.
- **Binário:** `ledger-service serve | migrate | healthcheck`.

### Configuração (variáveis de ambiente; segredos também via `NOME_FILE`)

| Variável | Descrição |
|---|---|
| `LEDGER_ENV` | `production` (padrão) ou `development`. Produção exige DB com `sslmode=verify-full\|verify-ca`, Kafka com TLS e assinatura HMAC |
| `LEDGER_DATABASE_URL[_FILE]` | DSN do papel `ledger_app` (serviço) |
| `LEDGER_MIGRATE_DATABASE_URL[_FILE]` | DSN do dono do schema (somente `ledger-service migrate`) |
| `LEDGER_TLS_CERT_FILE`, `LEDGER_TLS_KEY_FILE`, `LEDGER_TLS_CLIENT_CA_FILE` | mTLS obrigatório (TLS 1.3); o certificado é relido a cada conexão (rotação sem reinício) |
| `LEDGER_ACL_FILE` | JSON `{"<CN ou URI SAN>": ["Credit", …]}`; sem entrada = negado; arquivo vazio recusa subir |
| `LEDGER_CURRENCIES` | moedas habilitadas (padrão `BRL`) |
| `LEDGER_KAFKA_BROKERS`, `LEDGER_KAFKA_TLS_CA_FILE`, `LEDGER_KAFKA_SASL_USER`, `LEDGER_KAFKA_SASL_PASSWORD[_FILE]` | mensageria (vazio = desligada) |
| `LEDGER_HMAC_KEYS_FILE` | `{"signing_key_id":"k1","keys":{"k1":"<base64 ≥32 bytes>"}}`; várias chaves permitem rotação |
| `LEDGER_OPS_ADDR` | health/métricas (padrão `127.0.0.1:9090`; não exponha publicamente) |

### Qualidade e testes

- Unitários de domínio e eventos; integração do app (concorrência e propriedade), do PostgreSQL (invariantes no banco),
  do gRPC (mTLS/ACL) e e2e Kafka (comando assinado → ledger → outbox → evento assinado, replay e DLQ).
- `make test` (sem infraestrutura) e `make test-integration` (`-race`, PostgreSQL + Kafka descartáveis).

## Modelagem do domínio

Valores em **centavos (`bigint`)**, moeda ISO-4217. Partidas dobradas: em cada journal, débitos = créditos.

| Lançamento | Débito | Crédito |
|---|---|---|
| Hold | CUSTOMER | SETTLEMENT_TRANSIT |
| Settle | SETTLEMENT_TRANSIT | RESERVE |
| Release | SETTLEMENT_TRANSIT | CUSTOMER |
| Credit | RESERVE | CUSTOMER |
| Transfer | origem | destino |

CUSTOMER, TRANSIT e FEES são credit-normal; RESERVE e INTERNAL são debit-normal. Contas de sistema
(`system:<TYPE>:<CUR>`, `allow_negative`) são provisionadas na subida.

### Diagrama de classes (UML)

```mermaid
classDiagram
    class Account {
      +uuid id
      +string external_ref
      +AccountType type
      +Side normal_side
      +char3 currency
      +bool allow_negative
      +Status status
      +int64 balance
      +int64 version
    }
    class Journal {
      +uuid id
      +Kind kind
      +string reference_id
      +jsonb metadata
      +xid8 tx_id
    }
    class Entry {
      +uuid id
      +int64 seq
      +Direction direction
      +int64 amount
      +int64 balance_after
    }
    class Hold {
      +uuid id
      +string reference_id
      +int64 amount
      +HoldStatus status
    }
    class CommandLog {
      +string reference_id
      +Command command
      +string request_hash
      +Outcome outcome
      +jsonb result
    }
    class Outbox {
      +int64 id
      +string topic
      +string msg_key
      +bytea payload
      +timestamptz published_at
    }
    class Inbox {
      +string message_id
      +string consumer
    }
    Journal "1" *-- "2..*" Entry
    Account "1" o-- "*" Entry
    Account "1" o-- "*" Hold
    Journal "0..1" <-- Hold
    Journal "0..1" <-- CommandLog
```

### Modelo entidade-relacionamento

```mermaid
erDiagram
    ledger_account ||--o{ entry : movimenta
    journal ||--|{ entry : "contem (D=C)"
    ledger_account ||--o{ hold : reserva
    journal ||--o| hold : origina
    journal ||--o| command_log : resultado
    ledger_account ||--o{ balance_snapshot : diario
    outbox }o--o| journal : "mesma transacao"
```

### Estados do `Hold`

```mermaid
stateDiagram-v2
    [*] --> HELD: Hold
    [*] --> CANCELLED: Hold tardio após Release (NOOP)
    HELD --> SETTLED: Settle
    HELD --> RELEASED: Release
    SETTLED --> [*]
    RELEASED --> [*]
    CANCELLED --> [*]
```

## Fluxos

### Comando assíncrono (idempotência, outbox, inbox)

```mermaid
sequenceDiagram
    participant P as payment-service
    participant K as Kafka
    participant C as Consumer (ledger)
    participant S as app.Service
    participant DB as PostgreSQL
    participant R as Outbox relay
    P->>K: ledger.hold.requested (HMAC)
    K->>C: mensagem
    C->>C: verifica assinatura e JSON estrito
    alt inválida
      C->>K: DLQ (motivo)
    else válida
      C->>S: HandleCommand (inbox)
      S->>DB: BEGIN, lock da referência, consulta command_log
      alt já processado
        S-->>C: resultado gravado (reemite evento)
      else novo
        S->>DB: journal + entries (trigger aplica saldo) + hold
        S->>DB: command_log + outbox (mesma transação)
      end
      S->>DB: COMMIT
    end
    R->>DB: lê outbox não publicado
    R->>K: ledger.hold.succeeded ou failed (HMAC)
    K-->>P: evento
```

### Saga de pagamento (alvo)

Detalhes em [Docs/04-sequences.md](Docs/04-sequences.md) e [ADR-0006](Docs/0006-saga-tcc.md).

```mermaid
sequenceDiagram
    participant U as Cliente
    participant P as payment-service
    participant L as ledger-service
    participant X as SPI/RSFN
    U->>P: iniciar Pix
    P->>L: Hold (Try)
    L-->>P: hold.succeeded
    P->>X: enviar ordem
    alt confirmada
      X-->>P: liquidada
      P->>L: Settle (Confirm)
      L-->>P: settled
    else rejeitada ou timeout
      P->>L: Release (Cancel)
      L-->>P: released
    end
```

### Componentes de implantação (dev)

```mermaid
flowchart LR
    subgraph compose[docker-compose]
      ls[ledger-service<br/>distroless, nonroot]
      pg[(PostgreSQL)]
      kf[[Kafka]]
    end
    cli[ledgerctl / SDK Go] -->|mTLS gRPC| ls
    ls -->|sslmode + papel ledger_app| pg
    ls <-->|TLS/SASL + HMAC| kf
    ops[Prometheus] -.rede interna.-> ls
```

## Modelo de segurança

Detalhes e limites em [Docs/ledger-security.md](Docs/ledger-security.md).

```mermaid
flowchart LR
    A[mTLS 1.3 + ACL por CN/SAN] --> B[Rate limit, timeout, recover]
    B --> C[JSON estrito + validação de domínio]
    C --> D[Idempotência + advisory lock por referência]
    D --> E[Constraints e triggers no PostgreSQL]
    E --> F[Journal imutável + papel mínimo ledger_app]
    F --> G[Verificação contínua de invariantes]
```

- Saldo alterado apenas por trigger `SECURITY DEFINER`; partida dobrada garantida por constraint trigger deferido.
- `UPDATE/DELETE/TRUNCATE` bloqueados em `journal`, `entry` e `command_log`, inclusive para o dono.
- Mensagens Kafka assinadas com HMAC (chaves rotacionáveis); falha de assinatura vai para a DLQ.
- Configuração de produção falha fechada (sem TLS/ACL/HMAC o serviço não sobe).

## Limitações conhecidas

- O plano de contas é simplificado e **precisa de revisão contábil** antes de uso real.
- `SETTLEMENT_TRANSIT` e `RESERVE` são linhas quentes (serializam `Hold`/`Settle` por moeda); fragmentação fica para a etapa de carga.
- Sem teste de carga, restore ensaiado ou revisão de segurança externa: o componente é **Beta** até cumprir o checklist em [production-readiness](Docs/production-readiness.md).
- Operar Pix real exige autorização do Banco Central; o projeto não substitui conformidade regulatória.
