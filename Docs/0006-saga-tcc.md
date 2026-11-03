# ADR-0006: Saga orquestrada com TCC no ledger

- **Status:** Proposto
- **Data:** 2026-11-02

## Contexto
Sem transação ACID entre `payment-service` e `ledger-service`, é preciso consistência eventual sem perder nem duplicar dinheiro.

## Decisão
1. O **`payment-service` orquestra** a Saga e concentra a máquina de estados. O `ledger-service` expõe operações **TCC** neutras (sem conceitos de Pix):

| Operação | Papel | Journal |
|---|---|---|
| `Hold` | Try: valida saldo e retém | `HOLD` |
| `Settle` | Confirm | `SETTLEMENT` |
| `Release` | Cancel (estorno) | `REVERSAL` |
| `Transfer` | movimento interno único | `INTERNAL_TRANSFER` |
| `Credit` | crédito externo (origem como metadado opaco) | `EXTERNAL_CREDIT` |

2. `Hold`, `Settle`, `Release` e `Transfer` são **comandos por evento** (outbox/inbox). `Credit` é **gRPC síncrono**, pois o SPI exige resposta rápida no recebimento.
3. **Idempotência lógica**: `command_log (reference_id, command)` registra **sucesso e falha**. Reentrega devolve o resultado gravado e reemite o evento. Sem isso, um `Hold` que falhou por saldo poderia ter sucesso numa reentrega tardia e vazar dinheiro.
4. **Empty rollback e suspensão**: `Release` sem `Hold` grava `NOOP`; um `Hold` que chegar depois enxerga o `NOOP` e responde `CANCELLED` sem lançar.
5. **Timeout ≠ falha**: sem resposta, reenvia o mesmo comando ([ADR-0005](0005-unknown-state-and-reconciliation.md)).
6. **Estados intermediários visíveis**: `PENDING_HOLD`, `REVERSING`. A API é honesta sobre a consistência eventual.
7. Efeito sobre o cliente: o dinheiro sai no `Hold`. `Settle` é contabilidade (trânsito → reserva), então o pagamento é `SETTLED` assim que o SPI confirma, e o `Settle` segue com retry. A devolução só acontece no `Release`, por isso existe `REVERSING` antes de `REJECTED`.
8. **`reference_id`** é genérico (o ledger não conhece "pagamento"); o `payment-service` usa o `payment_id` como `reference_id`.

## Alternativas
| Alternativa | Por que não |
|---|---|
| Coreografia pura | Fluxo disperso, difícil de operar e de auditar |
| 2PC / XA | Acoplamento, bloqueio e indisponibilidade em cascata |
| Ledger dentro do `payment-service` | Perde o isolamento e a reutilização do ledger |

## Consequências
- (+) Cada passo tem dono, estado e retry; falhas ficam enumeradas ([matriz de falhas](../architecture/05-failure-matrix.md)).
- (−) Mais estados e testes (caos obrigatório).
