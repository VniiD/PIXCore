# Diagramas de sequência

Legenda: `Ledger` = `ledger-service`; `Pay` = `payment-service`; `GW` = `spi-gateway`; `Bus` = Kafka. Mensagens por `Bus` passam por outbox/inbox ([ADR-0003](../adr/0003-outbox-inbox.md)).

## 1. Pagamento enviado

### 1.1 Caminho feliz

```mermaid
sequenceDiagram
    autonumber
    actor C as Cliente (via banco)
    participant E as Edge
    participant Pay as payment-service
    participant Bus as Kafka
    participant L as ledger-service
    participant GW as spi-gateway
    participant S as SPI (ou simulador)

    C->>E: POST /v1/payments (Idempotency-Key, JWT)
    E->>Pay: request com claims
    Pay->>Pay: valida conta ACTIVE, limites, idempotência
    Pay->>Pay: grava payment PENDING_HOLD + consumo de limite + outbox (1 transação)
    Pay-->>C: 202 Accepted (PENDING_HOLD)
    Pay->>Bus: ledger.hold.requested (reference_id = payment_id)
    Bus->>L: comando
    L->>L: valida saldo, lança HOLD, grava command_log SUCCESS + outbox
    L->>Bus: ledger.hold.succeeded
    Bus->>Pay: evento
    Pay->>Pay: estado HELD
    Pay->>GW: SendPayment (end_to_end_id)
    GW->>S: pacs.008 / API
    S-->>GW: aceito
    GW-->>Pay: SENT
    S-->>GW: liquidado (confirmação)
    GW->>Bus: spi.payment.confirmed
    Bus->>Pay: evento
    Pay->>Pay: estado SETTLED, settle_status PENDING
    Pay->>Bus: ledger.settle.requested
    Bus->>L: comando
    L->>L: SETTLEMENT (trânsito para reserva), Hold SETTLED
    L->>Bus: ledger.settled
    Bus->>Pay: evento
    Pay->>Pay: settle_status DONE
```

### 1.2 Saldo insuficiente

```mermaid
sequenceDiagram
    autonumber
    actor C as Cliente
    participant Pay as payment-service
    participant Bus as Kafka
    participant L as ledger-service

    C->>Pay: POST /v1/payments
    Pay-->>C: 202 (PENDING_HOLD)
    Pay->>Bus: ledger.hold.requested
    Bus->>L: comando
    L->>L: saldo insuficiente, grava command_log FAILURE (sem lançamento)
    L->>Bus: ledger.hold.failed (INSUFFICIENT_FUNDS)
    Bus->>Pay: evento
    Pay->>Pay: REJECTED, libera consumo de limite
    C->>Pay: GET /v1/payments/{id}
    Pay-->>C: REJECTED (INSUFFICIENT_FUNDS)
    Note over L: Reentrega tardia do mesmo Hold devolve FAILURE gravado, nunca lança
```

### 1.3 Rejeição pelo SPI

```mermaid
sequenceDiagram
    autonumber
    participant Pay as payment-service
    participant GW as spi-gateway
    participant S as SPI
    participant Bus as Kafka
    participant L as ledger-service

    Note over Pay: estado HELD, Hold já lançado
    Pay->>GW: SendPayment
    GW->>S: envia
    S-->>GW: rejeitado (erro definitivo)
    GW-->>Pay: erro DEFINITIVO
    Pay->>Pay: estado REVERSING
    Pay->>Bus: ledger.release.requested
    Bus->>L: comando
    L->>L: REVERSAL (trânsito para cliente), Hold RELEASED
    L->>Bus: ledger.released
    Bus->>Pay: evento
    Pay->>Pay: REJECTED, libera consumo de limite
```

### 1.4 Timeout, UNKNOWN e reconciliação

```mermaid
sequenceDiagram
    autonumber
    participant Pay as payment-service
    participant Rec as Reconciliador (Pay)
    participant GW as spi-gateway
    participant S as SPI
    participant Bus as Kafka
    participant L as ledger-service

    Note over Pay: estado HELD
    Pay->>GW: SendPayment
    GW->>S: envia
    S--xGW: sem resposta (timeout)
    GW-->>Pay: erro DESCONHECIDO
    Pay->>Pay: estado UNKNOWN (Hold mantido)
    loop backoff exponencial
        Rec->>GW: GetPaymentStatus(end_to_end_id)
        GW->>S: consulta
        S-->>GW: status
        GW-->>Rec: status
    end
    alt Liquidado
        Rec->>Pay: SETTLED
        Pay->>Bus: ledger.settle.requested
    else Não liquidado (definitivo)
        Rec->>Pay: REVERSING
        Pay->>Bus: ledger.release.requested
        Bus->>L: Release
        L->>Bus: ledger.released
        Bus->>Pay: REJECTED
    else Esgotou tentativas
        Rec->>Pay: fila de intervenção manual + alerta (Hold mantido)
    end
```

### 1.5 Reenvio de comando sem resposta do ledger

```mermaid
sequenceDiagram
    participant Pay as payment-service
    participant Bus as Kafka
    participant L as ledger-service
    Pay->>Bus: ledger.hold.requested (ref=P1)
    Bus->>L: entrega 1
    L->>L: lança HOLD, command_log SUCCESS
    Note over L,Pay: evento de resposta se perde ou atrasa
    Pay->>Bus: reenvia ledger.hold.requested (ref=P1)
    Bus->>L: entrega 2
    L->>L: command_log já existe, não lança de novo
    L->>Bus: reemite ledger.hold.succeeded (resultado gravado)
    Bus->>Pay: evento (inbox ignora duplicata se já processado)
```

## 2. Transferência interna (sem Saga)

```mermaid
sequenceDiagram
    autonumber
    actor C as Cliente
    participant Pay as payment-service
    participant Bus as Kafka
    participant L as ledger-service

    C->>Pay: POST /v1/payments (destino: conta do mesmo sistema)
    Pay->>Pay: valida contas ACTIVE, limites
    Pay-->>C: 202 (PENDING_HOLD)
    Pay->>Bus: ledger.transfer.requested
    Bus->>L: comando
    alt saldo suficiente
        L->>L: journal INTERNAL_TRANSFER (1 lançamento), command_log SUCCESS
        L->>Bus: ledger.transferred
        Bus->>Pay: SETTLED
    else saldo insuficiente
        L->>Bus: ledger.transfer.failed
        Bus->>Pay: REJECTED
    end
```

## 3. Pix recebido

```mermaid
sequenceDiagram
    autonumber
    participant S as SPI
    participant GW as spi-gateway
    participant Pay as payment-service
    participant A as account-service
    participant L as ledger-service
    participant Bus as Kafka

    S->>GW: pagamento recebido (prazo curto de resposta)
    GW->>GW: valida mensagem, idempotência por end_to_end_id
    GW->>Pay: ReceiveIncoming (gRPC)
    Pay->>A: resolve conta destino e estado (AccountDirectory)
    alt conta apta
        Pay->>L: Credit (gRPC síncrono, reference_id = end_to_end_id, metadata de origem)
        L->>L: EXTERNAL_CREDIT, command_log SUCCESS
        L-->>Pay: ok
        Pay->>Pay: registra pagamento recebido
        Pay-->>GW: ACEITO
        GW-->>S: confirma
        Pay->>Bus: payment.received
    else conta inexistente ou não apta
        Pay-->>GW: RECUSADO (motivo)
        GW-->>S: devolve/recusa
    end
    Note over L: Reentrega do mesmo end_to_end_id devolve resultado gravado
```

## 4. Cadastro de chave Pix

```mermaid
sequenceDiagram
    autonumber
    actor C as Cliente
    participant A as account-service
    participant GW as spi-gateway
    participant D as DICT (ou simulador)
    participant Bus as Kafka

    C->>A: POST /v1/keys (tipo, valor)
    A->>A: conta ACTIVE? chave livre localmente?
    A->>A: grava pix_key PENDING + outbox
    A-->>C: 202 (PENDING)
    A->>GW: RegisterKey
    GW->>D: registra
    alt registrada
        D-->>GW: ok
        GW-->>A: ok
        A->>A: pix_key ACTIVE
        A->>Bus: account.key.registered
    else conflito/rejeitada
        D-->>GW: erro definitivo
        GW-->>A: erro
        A->>A: pix_key REJECTED
    else erro desconhecido
        GW-->>A: desconhecido
        A->>A: mantém PENDING, revalida via ResolveKey (mesma lógica de UNKNOWN)
    end
```
