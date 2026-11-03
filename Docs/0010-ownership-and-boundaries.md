# ADR-0010: Fronteiras de dados: limites, contas e chaves

- **Status:** Proposto
- **Data:** 2026-11-02

## Contexto
Os textos iniciais divergiam sobre quem é dono de limites Pix e da "conta". Esta decisão fecha essas fronteiras.

## Decisão
| Tema | Dono | Regra |
|---|---|---|
| Regra e consumo de **limites Pix** (por transação, diário, janela noturna) | `payment-service` | O limite precisa ser verificado junto com o pagamento. Contadores atualizados na mesma transação que cria o pagamento |
| **Perfil do cliente** (estado da conta, parâmetros de limite) | `account-service` (ou o serviço de contas do adotante via `AccountDirectory`) | O `payment-service` consulta ou replica por evento (`AccountStatusChanged`, `LimitProfileChanged`) |
| **Conta de negócio** (titular, estado, ciclo de vida) | `account-service` | Estados: `PENDING_KYC`, `ACTIVE`, `BLOCKED`, `CLOSED` |
| **Conta contábil** (`ledger_account`) | `ledger-service` | Só `external_ref` opaco; sem PII |
| **Chaves Pix** | `account-service`; registro no DICT via `spi-gateway` | Só conta `ACTIVE` pode registrar chave |

**Regras de negócio derivadas:**
- Pagamento exige conta de origem `ACTIVE`; recebimento exige conta destino que aceite crédito (`ACTIVE`, ou `BLOCKED` conforme política configurável).
- Encerramento de conta respeita LGPD (anonimização de PII no `account-service`) e retenção contábil (journals do ledger são mantidos, pois não têm PII).
- Se a réplica de estado de conta estiver desatualizada, o `payment-service` aplica a política **fail-closed** para débitos.

## Consequências
- (+) Cada regra tem um único dono; ledger permanece neutro.
- (−) Réplica de perfil por evento exige tratamento de atraso (fail-closed) e testes de contrato.
