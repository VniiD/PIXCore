# Política de segurança

## Reportar vulnerabilidades
Não abra issue pública. Use *Security Advisories* do GitHub (aba Security → Report a vulnerability).
Meta: confirmação em 2 dias úteis, correção de severidade alta/crítica em até 15 dias.

## Segredos
Nenhum segredo é versionado. Segredos chegam por arquivo (`NOME_FILE`) vindos de um cofre (Vault/KMS/Secrets Manager).
`gitleaks` roda no CI sobre todo o histórico e `make security` localmente. Se um segredo vazar: revogar/rotacionar
primeiro (ver [runbook](Docs/runbooks/incident-and-key-rotation.md)), depois remover do histórico.

## Versões suportadas
Somente a `main`.
