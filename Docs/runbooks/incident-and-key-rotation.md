# Runbook: incidentes e rotação de chaves

Apoia ISO/IEC 27001:2022 A.5.24–A.5.28 (incidentes), A.8.24 (criptografia) e A.5.17 (autenticação).

## 1. Rotação de chave HMAC (sem indisponibilidade)
1. Gerar nova chave (≥32 bytes aleatórios, `openssl rand -base64 48`) no cofre.
2. Publicar `hmac_keys` com **as duas** chaves e `signing_key_id` ainda na antiga; reiniciar em rolling todos os serviços (verificam ambas).
3. Trocar `signing_key_id` para a nova; reiniciar em rolling.
4. Após a retenção máxima das mensagens Kafka, remover a chave antiga.
Vazamento suspeito: executar 1–3 imediatamente e remover a antiga logo após esvaziar a DLQ/lag.

## 2. Rotação de certificados mTLS
O serviço relê o certificado a cada conexão: substituir `cert/key` no volume/secret (cert-manager) sem reinício.
CA comprometida: emitir nova CA, distribuir como `LEDGER_TLS_CLIENT_CA_FILE` (bundle antiga+nova), reemitir certificados, remover a antiga.

## 3. Credenciais de banco
Trocar a senha do papel `ledger_app` (`ALTER ROLE … PASSWORD`), atualizar o segredo `database_url` e reiniciar. O dono do schema só é usado em `migrate`.

## 4. Segredo vazado em repositório
1. Revogar/rotacionar (seções acima) **antes** de qualquer limpeza.
2. Identificar uso indevido nos logs de `rpc`/`access denied` (campos `identity`, `method`, `code`).
3. Remover do histórico (`git filter-repo`) e forçar push; invalidar clones/caches.
4. Registrar o incidente e a lição aprendida (A.5.27).

## 5. Alertas a observar
`access denied` em rajada, mensagens na DLQ por assinatura inválida, violações de invariantes (métricas `/metrics`), `outbox lag` crescente.

## 6. Classificação e comunicação
Severidade 1 (integridade do ledger/vazamento de chave): acionar responsável de segurança, congelar contas afetadas (`SetAccountStatus FROZEN`), preservar logs e comunicar conforme LGPD/regulador em prazo legal.
