# ADR-0009: Ledger de partidas dobradas, genérico

- **Status:** Proposto
- **Data:** 2026-11-02

## Contexto
O ledger sustentará o PixCore e, depois, um banco digital inteiro. Ele precisa nascer como ledger de banco, sem conceitos de Pix; retrofit de contabilidade custa muito mais que fazer agora.

## Decisão
1. **Partidas dobradas:** todo `journal` tem `entries` cuja soma de débitos = soma de créditos, na mesma moeda. Invariante verificada em código e por constraint/trigger de fechamento da transação.
2. **Imutabilidade:** `journal` e `entry` são *append-only*. Correção se faz por novo journal (`REVERSAL`), nunca por UPDATE/DELETE. Permissões do banco impedem UPDATE/DELETE nessas tabelas.
3. **Valores** em unidade menor (`bigint`, centavos) + `currency`. Sem ponto flutuante.
4. **Plano de contas** (`ledger_account.account_type`): `CUSTOMER`, `SETTLEMENT_TRANSIT`, `RESERVE`, `FEES`, `INTERNAL`. O plano inicial é simplificado e deve ser revisado por contador antes de uso real.
5. **`ledger_account`** tem apenas `external_ref` opaco (id da conta de negócio, vindo do `account-service` ou do sistema do adotante). **Sem dados pessoais** no ledger.
6. **Saldos:** disponível (`balance`), retido (soma de `hold` abertos) e contábil (= disponível + retido). `Hold` debita a conta do cliente e credita `SETTLEMENT_TRANSIT`, então o disponível já cai no `Hold`.
7. **Não-negatividade:** contas `CUSTOMER` não podem ficar negativas (`allow_negative = false`); a checagem e a atualização do saldo ocorrem com lock de linha na mesma transação do lançamento.
8. **Concorrência:** `SELECT ... FOR UPDATE` na conta, com ordenação determinística de locks (por id) em movimentos com duas contas, para evitar deadlock.
9. **Read models:** eventos de lançamento suficientes para o consumidor montar extrato; o ledger não implementa extrato rico.
10. **Snapshots e fechamento diário:** `balance_snapshot` por conta e dia, base para conciliação contábil. Verificação diária: soma de todos os saldos = 0 por moeda (considerando natureza das contas).
11. **Idempotência:** `command_log` conforme [ADR-0006](0006-saga-tcc.md).

## Consequências
- (+) Auditável, reconstruível a partir dos journals, reutilizável por qualquer sistema que mova dinheiro.
- (−) Mais tabelas e rigor; testes de propriedade (soma zero, saldo = soma das entries, idempotência) são obrigatórios.
