# Segurança do `ledger-service`

Princípios: negar por padrão, defesa em profundidade, menor privilégio, falhar rápido na configuração.

## Controles implementados

| Camada | Controle | Evidência (teste) |
|---|---|---|
| Transporte gRPC | mTLS obrigatório, TLS 1.3, sem tickets de sessão; certificado relido sem reinício | `adapters/grpc` `TestMTLSAndACL` (sem cert e CA estranha rejeitados) |
| Autorização | ACL por identidade do certificado × método, negado por padrão; ACL vazia impede subir | `TestMTLSAndACL` |
| Abuso | rate limit por identidade, timeout por requisição, limite de mensagem (1 MiB), `recover` sem vazar detalhes | revisão de código |
| Validação | moeda (allowlist), valor (1…10¹⁵), `reference_id`/`external_ref` por regex, metadata limitado, JSON estrito nos comandos | `domain` testes, `HandleCommand` |
| Integridade financeira | partidas dobradas por trigger deferido; saldo só muda por trigger `SECURITY DEFINER`; `CHECK` de não-negatividade; `UNIQUE(reference_id, kind)` por journal; entradas só na transação que criou o journal | `adapters/postgres` `TestDatabaseEnforcesLedgerInvariants` |
| Imutabilidade | triggers bloqueiam UPDATE/DELETE/TRUNCATE em `journal`, `entry`, `command_log` (inclusive para o dono); papel `ledger_app` sem esses privilégios | idem |
| Idempotência | `command_log` com hash da requisição: reuso de `reference_id` com corpo diferente é rejeitado | `TestHoldReplayIdempotentAndMismatch` |
| Concorrência | lock de linha ordenado por id + advisory lock por `reference_id`; retry em deadlock/serialização | `TestConcurrent*`, `TestPropertyRandomOperations` (`-race`) |
| Mensageria | CloudEvents com HMAC-SHA256 sobre os bytes exatos (outbox `bytea`), `kid` para rotação, comparação em tempo constante, assinatura obrigatória em produção, DLQ para forjadas/inválidas | `adapters/kafka` `TestCommandsToEventsEndToEnd` |
| Configuração | produção recusa DB sem `verify-*`, Kafka sem TLS, mensageria sem HMAC; segredos por arquivo | `config` testes |
| Dados pessoais | ledger só guarda `external_ref` opaco; logs redigem campos sensíveis | revisão de código |
| Contêiner | imagem distroless, usuário não-root, FS somente leitura, `cap_drop: ALL`, `no-new-privileges`, ops em loopback | `docker compose` |
| Operação | métricas das invariantes (`ledger_invariant_violations`), outbox/DLQ; migrações com checksum e lock | `Verify` nos testes |

## Fora do escopo desta etapa (pendente)

- Revisão de segurança independente e modelo de ameaças formal.
- Gestão de segredos/PKI de produção (Vault/cert-manager), rotação automatizada e revogação (CRL/OCSP).
- Autenticação/autorização de usuários finais (feita no edge, [ADR-0001](0001-microservices-eda-hexagonal.md)).
- Trilha de auditoria imutável externa (`audit-notification-service`, etapa posterior).
- mTLS/ACL do Kafka no broker (SASL/TLS são suportados pelo cliente; a política de ACL do broker é da infraestrutura).
