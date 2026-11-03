# Modelo do `ledger-service`

Genérico, sem conceitos de Pix ([ADR-0009](../adr/0009-ledger-design.md)).

## Modelo de domínio (UML de classes)

```mermaid
classDiagram
    class LedgerAccount {
        +UUID id
        +string externalRef
        +AccountType type
        +string currency
        +bool allowNegative
        +AccountStatus status
        +int64 balance
        +int64 version
    }
    class Journal {
        +UUID id
        +JournalKind kind
        +string referenceId
        +datetime createdAt
        +json metadata
    }
    class Entry {
        +UUID id
        +Direction direction
        +int64 amount
        +string currency
    }
    class Hold {
        +UUID id
        +string referenceId
        +int64 amount
        +HoldStatus status
    }
    class CommandLog {
        +string referenceId
        +Command command
        +Outcome outcome
        +json result
    }
    class BalanceSnapshot {
        +date day
        +int64 balance
        +int64 held
    }
    class AccountType {
        <<enumeration>>
        CUSTOMER
        SETTLEMENT_TRANSIT
        RESERVE
        FEES
        INTERNAL
    }
    class JournalKind {
        <<enumeration>>
        HOLD
        SETTLEMENT
        REVERSAL
        INTERNAL_TRANSFER
        EXTERNAL_CREDIT
    }
    class HoldStatus {
        <<enumeration>>
        HELD
        SETTLED
        RELEASED
        CANCELLED
    }
    class Outcome {
        <<enumeration>>
        SUCCESS
        FAILURE
        NOOP
    }
    Journal "1" *-- "2..*" Entry : contém
    LedgerAccount "1" --> "0..*" Entry : recebe
    LedgerAccount "1" --> "0..*" Hold : retém
    Hold "1" --> "1" Journal : lançado por
    CommandLog "1" --> "0..1" Journal : resultado
    LedgerAccount "1" --> "0..*" BalanceSnapshot
```

**Invariantes:** (1) por journal, Σ débitos = Σ créditos; (2) `balance` da conta = Σ entries da conta (com sinal pela natureza); (3) `CUSTOMER` com `allowNegative=false` nunca fica negativa; (4) no máximo um `CommandLog` por (`referenceId`, `command`); (5) entries e journals nunca mudam.

## Efeito das operações no razão

Convenção simplificada (a revisar por contador): `CUSTOMER` é passivo do banco; `SETTLEMENT_TRANSIT` é conta transitória; `RESERVE` representa a reserva de liquidação.

| Operação | Débito | Crédito | Hold | Observação |
|---|---|---|---|---|
| `Hold` | `CUSTOMER` | `SETTLEMENT_TRANSIT` | cria `HELD` | Valida saldo disponível; disponível cai |
| `Settle` | `SETTLEMENT_TRANSIT` | `RESERVE` | `HELD` → `SETTLED` | Contabilidade; sem efeito no disponível do cliente |
| `Release` | `SETTLEMENT_TRANSIT` | `CUSTOMER` | `HELD` → `RELEASED` | Estorno; sem `Hold` prévio grava `NOOP` e marca suspensão |
| `Transfer` | `CUSTOMER` (origem) | `CUSTOMER` (destino) | n/a | Um único journal `INTERNAL_TRANSFER` |
| `Credit` | `RESERVE` | `CUSTOMER` | n/a | Crédito externo; origem em `metadata` opaco |

## Máquina de estados do `Hold`

```mermaid
stateDiagram-v2
    [*] --> HELD : Hold ok
    [*] --> CANCELLED : Hold chega após Release (NOOP)
    HELD --> SETTLED : Settle
    HELD --> RELEASED : Release
    SETTLED --> [*]
    RELEASED --> [*]
    CANCELLED --> [*]
    note right of HELD
        Release após SETTLED é rejeitado
        (decisão do orquestrador, ADR-0005)
    end note
```

## Modelo de dados (PostgreSQL do ledger)

```mermaid
erDiagram
    ledger_account ||--o{ entry : "tem"
    journal ||--|{ entry : "contém"
    ledger_account ||--o{ hold : "retém"
    journal ||--o| hold : "origina"
    command_log }o--o| journal : "resultado"
    ledger_account ||--o{ balance_snapshot : "fecha"

    ledger_account {
        uuid id PK
        text external_ref UK
        text account_type
        char3 currency
        boolean allow_negative
        text status
        bigint balance
        bigint version
        timestamptz created_at
    }
    journal {
        uuid id PK
        text kind
        text reference_id
        jsonb metadata
        timestamptz created_at
    }
    entry {
        uuid id PK
        uuid journal_id FK
        uuid account_id FK
        text direction
        bigint amount
        char3 currency
        bigint balance_after
        timestamptz created_at
    }
    hold {
        uuid id PK
        text reference_id UK
        uuid account_id FK
        uuid journal_id FK
        bigint amount
        text status
        timestamptz created_at
    }
    command_log {
        text reference_id PK
        text command PK
        text outcome
        jsonb result
        uuid journal_id FK
        timestamptz created_at
    }
    balance_snapshot {
        uuid account_id PK
        date day PK
        bigint balance
        bigint held
    }
    outbox {
        bigint id PK
        text aggregate_id
        text topic
        text msg_key
        jsonb payload
        jsonb headers
        timestamptz created_at
        timestamptz published_at
    }
    inbox {
        text message_id PK
        text consumer PK
        timestamptz processed_at
    }
```

**Restrições principais:** `CHECK (amount > 0)` em `entry`; `CHECK (balance >= 0 OR allow_negative)` em `ledger_account`; `UNIQUE (reference_id)` em `hold`; PK composta `(reference_id, command)` em `command_log`; índices em `entry (account_id, created_at)` e `journal (reference_id)`; sem permissão de UPDATE/DELETE em `journal` e `entry`.

## Contrato (esboço)

```protobuf
service Ledger {
  rpc Credit(CreditRequest) returns (CreditResponse);          // síncrono
  rpc GetBalance(GetBalanceRequest) returns (Balance);         // disponível, retido, contábil
  rpc ListEntries(ListEntriesRequest) returns (EntryPage);
}
// Comandos assíncronos (tópico pixcore.ledger.commands.v1, CloudEvents):
//   ledger.hold.requested / settle.requested / release.requested / transfer.requested
// Eventos (tópico pixcore.ledger.events.v1):
//   ledger.hold.succeeded | hold.failed(INSUFFICIENT_FUNDS | CANCELLED)
//   ledger.settled | ledger.released | ledger.transferred | ledger.credited
```
