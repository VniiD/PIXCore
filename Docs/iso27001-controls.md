# Mapeamento ISO/IEC 27001:2022 (Anexo A) → PixCore

> **Aviso:** a certificação ISO 27001 é de uma **organização** (SGSI: política, análise de riscos, auditoria interna,
> auditoria externa). Código não "é" certificado. Este documento mostra os controles técnicos implementados e as
> lacunas organizacionais que continuam pendentes.

## Controles técnicos implementados

| Controle | Requisito | Implementação |
|---|---|---|
| A.5.15 / A.8.3 Controle de acesso | Mínimo privilégio | ACL por identidade mTLS (CN/URI SAN) por método gRPC, negado por padrão; papel de banco `ledger_app` com privilégios mínimos; dono do schema só em `migrate` |
| A.5.17 / A.8.5 Autenticação | Autenticação forte | mTLS TLS 1.3 obrigatório; SASL/SCRAM no Kafka; assinatura HMAC por mensagem |
| A.5.33 / A.8.15 Registros (logs) | Rastreabilidade | log estruturado de cada RPC (`identity`, `method`, `code`, duração) e negações; redação automática de campos sensíveis |
| A.8.24 Criptografia | Uso de criptografia | TLS 1.3, DB `verify-full` obrigatório em produção, Kafka TLS, HMAC com rotação de chave (ver runbook) |
| A.8.12 / A.5.34 Proteção de dados | Sem vazamento em logs | redator de `password/secret/token/key/dsn/cpf/cnpj`; segredos só por arquivo (`_FILE`) |
| A.8.9 Gestão de configuração | Configuração segura | produção falha fechada (sem TLS/ACL/HMAC o serviço não sobe); validação estrita |
| A.8.8 / A.8.28 Vulnerabilidades e código seguro | Varredura contínua | `govulncheck`, `gosec`, `gitleaks` (histórico), `trivy` no CI; Dependabot; `make security` |
| A.8.25 / A.8.29 Ciclo de desenvolvimento seguro | Testes de segurança | testes de mTLS/ACL, invariantes no banco, assinatura HMAC, DLQ, concorrência e propriedade (`-race`) |
| A.8.32 Gestão de mudanças | Mudanças controladas | migrações versionadas com checksum e advisory lock; CI obrigatório |
| A.8.13 Backup / A.8.14 Redundância | Recuperação | journal imutável e verificação contínua de invariantes; **restore ensaiado pendente** |
| A.8.20 / A.8.22 Segurança de rede | Segmentação | rede interna dedicada, ops (`/metrics`) em loopback, portas publicadas só em 127.0.0.1 (dev) |
| A.8.26 Requisitos de segurança de aplicação | Validação | JSON estrito (campos desconhecidos rejeitados), limites de tamanho, rate limit, timeouts, `recover` |
| A.8.7 Proteção contra malware / A.8.19 | Imagem mínima | distroless, `nonroot`, FS somente leitura, `cap_drop: ALL`, `no-new-privileges`, limites de PID/CPU/memória |
| A.8.16 Monitoramento | Detecção | métricas Prometheus (violações de invariantes, lag de outbox), health checks |
| A.5.24–A.5.28 Incidentes | Resposta | [runbook](runbooks/incident-and-key-rotation.md), DLQ, congelamento de conta |
| A.5.1/A.5.37 Documentação | Procedimentos | [SECURITY.md](../SECURITY.md), este mapeamento, [ledger-security](ledger-security.md) |
| A.8.10 Exclusão / A.8.11 | Retenção | expurgo de outbox/inbox; journal imutável por requisito contábil |

## Lacunas organizacionais (fora do código; necessárias para certificar)

- Escopo do SGSI, política de segurança aprovada pela direção, declaração de aplicabilidade (SoA).
- Análise e tratamento de riscos formais, métricas e revisão pela direção.
- Gestão de pessoas/fornecedores (A.6, A.5.19–A.5.22), segurança física (A.7), continuidade (A.5.29–A.5.30) com testes de DR.
- Cofre de segredos/KMS/HSM de produção e PKI corporativa; SIEM com retenção e alertas.
- Pentest e revisão externa; auditorias interna e externa (Estágios 1 e 2).
- Proteção em repouso (criptografia de disco/TDE) e backups cifrados na infraestrutura de produção.
