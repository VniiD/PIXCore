# Matriz de falhas

Cada falha tem detecção, efeito e tratamento. Todo item marcado com *(caos)* é coberto por teste de caos no CI.

## Pagamento enviado

| # | Falha | Detecção | Efeito no dinheiro | Tratamento |
|---|---|---|---|---|
| F1 | `payment-service` cai após gravar `PENDING_HOLD` e antes de publicar | outbox pendente | nenhum | Outbox publica após reinício *(caos)* |
| F2 | Broker indisponível | falha de publicação/lag | nenhum | Outbox retém; relay retoma; alerta de lag |
| F3 | Comando `Hold` duplicado | `command_log` | nenhum (sem duplicar) | Devolve resultado gravado e reemite evento *(caos)* |
| F4 | `Hold` falhou por saldo e chega de novo, tardiamente | `command_log` FAILURE | nenhum (sem vazamento) | Devolve FAILURE; nunca reavalia |
| F5 | `Release` chega antes do `Hold` | `Hold` inexistente | nenhum | Grava `NOOP`; `Hold` tardio responde `CANCELLED` *(caos)* |
| F6 | Evento de resposta do ledger se perde | timeout do orquestrador | nenhum | Reenvia o mesmo comando (idempotente) |
| F7 | Consumidor processa a mesma mensagem duas vezes | inbox | nenhum | Inbox ignora o efeito duplicado |
| F8 | Timeout no `SendPayment` | erro desconhecido | dinheiro retido (`Hold`) | `UNKNOWN`, reconciliação, sem `Release` cego *(caos)* |
| F9 | SPI rejeita definitivamente | erro definitivo | devolução | `REVERSING` → `Release` → `REJECTED` |
| F10 | `Release` falha repetidamente | retry esgotado | dinheiro retido | Alerta e fila manual; `REVERSING` permanece visível |
| F11 | `Settle` falha após `SETTLED` | `settle_status = PENDING` | contábil pendente, cliente já correto | Retry indefinido com alerta de idade |
| F12 | `ledger-service` indisponível | falha de comando | nenhum | Comandos se acumulam no broker; pagamento fica `PENDING_HOLD` |
| F13 | Reenvio de `SendPayment` | idempotência por `end_to_end_id` | nenhum | Gateway devolve resultado original |
| F14 | Estado da conta desatualizado no `payment-service` | idade da réplica | risco de débito indevido | Fail-closed para débitos acima do limiar de idade |
| F15 | Limite consumido e pagamento rejeitado | estado `REJECTED` | nenhum | Consumo de limite é liberado na rejeição |

## Pix recebido e demais fluxos

| # | Falha | Efeito | Tratamento |
|---|---|---|---|
| R1 | `Credit` com timeout | incerteza se creditou | Reenvio com mesmo `reference_id`; ledger devolve resultado gravado |
| R2 | SPI reentrega o mesmo recebimento | duplicar crédito | Idempotência por `end_to_end_id` no gateway e no `command_log` |
| R3 | Conta destino não apta | crédito indevido | Recusa ao SPI com motivo |
| K1 | `RegisterKey` com resultado desconhecido | chave em estado incerto | `PENDING`, revalidação por `ResolveKey` |
| T1 | `Transfer` duplicado | duplicar movimento | `command_log` |

## Invariantes monitoradas

| Invariante | Verificação | Alerta |
|---|---|---|
| Σ débitos = Σ créditos por journal | constraint + teste de propriedade | crítico |
| `balance` = Σ entries por conta | job periódico | crítico |
| Σ saldos por moeda consistente com plano de contas | fechamento diário | crítico |
| Nenhum `Hold` aberto além de limite de idade sem pagamento associado | job periódico | alto |
| `UNKNOWN` acima de limite de idade | métrica | alto |
| DLQ vazia | métrica | alto |
