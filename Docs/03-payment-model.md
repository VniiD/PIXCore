# Modelo do `payment-service`

## Modelo de domínio (UML de classes)

```mermaid
classDiagram
    class Payment {
        +UUID id
        +string endToEndId
        +UUID sourceAccountId
        +PaymentKind kind
        +int64 amount
        +string currency
        +PaymentState state
        +SettleStatus settleStatus
        +string failureReason
        +datetime createdAt
        +datetime updatedAt
    }
    class PaymentAttempt {
        +int seq
        +string step
        +string outcome
        +datetime at
    }
    class LimitPolicy {
        +UUID accountId
        +int64 perTransaction
        +int64 dailyLimit
        +int64 nightlyLimit
        +time nightStart
        +time nightEnd
    }
    class LimitUsage {
        +UUID accountId
        +date day
        +string window
        +int64 consumed
    }
    class ApiIdempotencyKey {
        +string key
        +string requestHash
        +json response
    }
    class ReconciliationCase {
        +UUID paymentId
        +int attempts
        +datetime nextAttemptAt
        +CaseState state
    }
    class PaymentState {
        <<enumeration>>
        PENDING_HOLD
        HELD
        SENT
        UNKNOWN
        SETTLED
        REVERSING
        REJECTED
    }
    class SettleStatus {
        <<enumeration>>
        NOT_REQUIRED
        PENDING
        DONE
    }
    Payment "1" --> "0..*" PaymentAttempt
    Payment "1" --> "0..1" ReconciliationCase
    LimitPolicy "1" --> "0..*" LimitUsage
    Payment ..> LimitUsage : consome
```

## Máquina de estados do pagamento enviado

```mermaid
stateDiagram-v2
    [*] --> PENDING_HOLD : POST /payments (202)
    PENDING_HOLD --> HELD : ledger.hold.succeeded
    PENDING_HOLD --> REJECTED : hold.failed (saldo insuficiente / cancelado)
    HELD --> SENT : SendPayment aceito
    SENT --> SETTLED : SPI confirma
    SENT --> REVERSING : SPI rejeita (definitivo)
    SENT --> UNKNOWN : erro desconhecido / timeout
    HELD --> REVERSING : falha definitiva no envio
    UNKNOWN --> SETTLED : reconciliação confirma liquidação
    UNKNOWN --> REVERSING : reconciliação confirma não liquidação
    UNKNOWN --> UNKNOWN : ainda incerto (backoff)
    REVERSING --> REJECTED : ledger.released
    SETTLED --> SETTLED : Settle com retry (settleStatus PENDING → DONE)
    SETTLED --> [*]
    REJECTED --> [*]
```

Regras: `Hold` é mantido em `UNKNOWN`; `Release` só após evidência definitiva ([ADR-0005](../adr/0005-unknown-state-and-reconciliation.md)). `SETTLED` é atingido quando o SPI confirma; o `Settle` do ledger é contabilidade e segue com retry sem alterar o estado visível.

## Modelo de dados (PostgreSQL do payment-service)

```mermaid
erDiagram
    payment ||--o{ payment_attempt : "registra"
    payment ||--o| reconciliation_case : "pode ter"
    limit_policy ||--o{ limit_usage : "consumo"

    payment {
        uuid id PK
        text end_to_end_id UK
        uuid source_account_id
        text kind
        bigint amount
        char3 currency
        text state
        text settle_status
        text failure_reason
        jsonb destination
        timestamptz created_at
        timestamptz updated_at
    }
    payment_attempt {
        uuid payment_id PK
        int seq PK
        text step
        text outcome
        timestamptz at
    }
    api_idempotency {
        text idempotency_key PK
        text request_hash
        int status_code
        jsonb response
        timestamptz expires_at
    }
    limit_policy {
        uuid account_id PK
        bigint per_transaction
        bigint daily_limit
        bigint nightly_limit
        time night_start
        time night_end
        timestamptz updated_at
    }
    limit_usage {
        uuid account_id PK
        date day PK
        text window PK
        bigint consumed
    }
    reconciliation_case {
        uuid payment_id PK
        int attempts
        timestamptz next_attempt_at
        text state
    }
    account_status_replica {
        uuid account_id PK
        text status
        timestamptz updated_at
    }
    outbox {
        bigint id PK
        text topic
        text msg_key
        jsonb payload
        timestamptz published_at
    }
    inbox {
        text message_id PK
        text consumer PK
    }
```

Idempotência da API: `Idempotency-Key` + hash do corpo; mesma chave com corpo diferente retorna `422`; mesma chave e corpo devolve a resposta original. O consumo de limite e a criação do pagamento ocorrem na **mesma transação** local.

## Modelo do `account-service` (referência)

```mermaid
erDiagram
    account ||--o{ pix_key : "possui"
    account {
        uuid id PK
        uuid customer_id
        text status
        text holder_name
        text document_hash
        timestamptz created_at
        timestamptz closed_at
    }
    pix_key {
        uuid id PK
        uuid account_id FK
        text key_type
        text key_value UK
        text status
        timestamptz created_at
    }
```

Estados da conta: `PENDING_KYC → ACTIVE ⇄ BLOCKED → CLOSED`. `CLOSED` dispara anonimização de PII, mantendo apenas identificadores técnicos.

## Contrato REST (esboço OpenAPI)

```yaml
openapi: 3.1.0
info: { title: PixCore Payment API, version: 1.0.0 }
paths:
  /v1/payments:
    post:
      summary: Cria pagamento Pix (assíncrono)
      parameters:
        - { name: Idempotency-Key, in: header, required: true, schema: { type: string } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [sourceAccountId, amount, destination]
              properties:
                sourceAccountId: { type: string, format: uuid }
                amount: { type: integer, description: "centavos" }
                currency: { type: string, default: BRL }
                destination:
                  oneOf:
                    - { $ref: '#/components/schemas/PixKeyDestination' }
                    - { $ref: '#/components/schemas/AccountDestination' }
      responses:
        "202": { description: Aceito; estado PENDING_HOLD }
        "422": { description: Idempotency-Key reutilizada com corpo diferente }
  /v1/payments/{id}:
    get:
      summary: Consulta estado (PENDING_HOLD, HELD, SENT, UNKNOWN, SETTLED, REVERSING, REJECTED)
```
