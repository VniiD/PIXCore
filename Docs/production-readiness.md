# Prontidão para produção

"Pronto para produção" precisa de **evidência**, não de adjetivo. Este documento lista o nível de maturidade de cada componente e as provas correspondentes. Estado atual: a **Etapa 1** (`ledger-service`) está implementada; as demais etapas seguem em desenho. Evidências do ledger: testes em `services/ledger` (ver [ledger-security.md](ledger-security.md)); restante pendente.

## Níveis
| Nível | Significado |
|---|---|
| **Experimental** | Especificado ou esqueleto; sem garantia; pode mudar sem aviso |
| **Beta** | Funciona e é testado em escopo limitado; contratos podem sofrer ajuste |
| **Estável** | Contratos versionados, testes completos, evidências publicadas |

## Maturidade por componente
| Componente | Nível-alvo | Evidência exigida | Estado |
|---|---|---|---|
| `ledger-service` | Estável | testes de propriedade (soma zero, saldo = Σ entries, idempotência), carga, restore ensaiado | Parcial: testes de propriedade/idempotência/concorrência e de invariantes no banco passam; carga e restore pendentes |
| Saga + `spi-simulator` | Estável | integração e caos (F1–F15) | Pendente |
| Reconciliação | Estável | cenário `make demo`, caos F8 | Pendente |
| `audit-notification-service` | Beta | testes de contrato, entrega de webhook com retry | Pendente |
| Adapter `provider` | Beta | suíte de conformidade contra provedor mockado | Pendente |
| SDK Go | Estável | gerado e testado por contrato | Parcial: cliente do ledger testado via `ledgerctl`/mTLS; testes de contrato pendentes |
| SDK .NET ou Python (um) | Beta | gerado e testado por contrato | Pendente |
| Adapter `rsfn` | Experimental | especificação e esqueleto; **sem teste contra o SPI real** | Pendente |

## Evidências exigidas (checklist)
- [ ] Relatório de teste de carga (throughput, latência p95/p99, comportamento sob saturação)
- [ ] Resultados dos testes de caos no CI (matriz de falhas)
- [ ] Restore de backup ensaiado, com RPO/RTO medidos
- [ ] Runbooks: `UNKNOWN` preso, DLQ, outbox atrasado, invariante violada, rotação de certificados
- [ ] Revisão de segurança (modelo de ameaças, gestão de segredos, mTLS, minimização de PII)
- [ ] Revisão do plano de contas por contador
- [ ] Suíte de conformidade do `SpiAdapter` passando
- [ ] SLOs e alertas versionados ([ADR-0004](adr/0004-observability.md))

## Limite regulatório (importante)
Operar Pix diretamente no SPI exige ser instituição autorizada pelo Banco Central. Um banco digital real precisa ser participante direto ou indireto (via patrocinador) e cumprir as exigências regulatórias (segurança, auditoria, prevenção a fraude e lavagem de dinheiro, entre outras). No portfólio, o fluxo roda no `simulator`. Este projeto **não** substitui autorização nem conformidade regulatória.

## Roadmap por etapas
| Etapa | Entrega | Critério de saída |
|---|---|---|
| 1 | `ledger-service` standalone + testes de propriedade + SDK Go | Invariantes verdes; carga básica |
| 2 | `payment-service` + `spi-simulator` com a Saga | Fluxos 1.1 a 1.3 e 2 de [04-sequences.md](architecture/04-sequences.md) |
| 3 | Reconciliação + `make demo` + caos no CI | F1–F15 cobertas |
| 4 | Adapters: `provider` (Beta), esqueleto `rsfn` | Suíte de conformidade |
| 5 | Hardening: carga, restore, runbooks, revisão de segurança | Checklist acima completo |
