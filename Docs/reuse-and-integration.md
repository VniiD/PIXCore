# Reutilização e integração

## Princípio
O PixCore é consumido **por contrato**. O banco digital (repositório separado) é o primeiro consumidor; se precisar de fork, o desenho falhou.

## O que é reutilizável
| Componente | Reutilização | Dependência de Pix |
|---|---|---|
| `ledger-service` | Carteira digital, marketplace, banco, qualquer sistema que mova dinheiro | Nenhuma |
| `payment-service` + `spi-gateway` | Pix plugado ao ledger por contrato | Sim, isolado aqui |
| `account-service` | Referência; substituível | Chaves Pix |

## Pontos de extensão (sem fork)
| Porta | Para que serve | Como substituir |
|---|---|---|
| `AccountDirectory` | Usar o sistema de contas do adotante | Implementar o contrato gRPC/eventos e apontar a configuração |
| `RiskEvaluator` | Antifraude próprio | Implementar a porta; resposta `ALLOW`/`DENY`/`REVIEW` |
| `Notifier` | Canal de notificação | Implementar a porta |
| `SpiAdapter` | Backend de liquidação | `GATEWAY_ADAPTER=simulator\|provider\|rsfn` |

## Integração mínima
1. Subir os serviços (`deploy/docker-compose` em dev; Helm chart em ambientes reais).
2. Configurar o edge para validar o JWT do seu provedor de identidade. O token deve conter `customer_id` e permissões.
3. Criar contas no ledger via `external_ref` (id da sua conta de negócio).
4. Gerar o cliente a partir do contrato (SDK Go; .NET ou Python em Beta).
5. Assinar os eventos (`pixcore.*.events.v1`) ou configurar webhooks para extrato, notificações e conciliação.

## Papel do banco digital
Implementa `AccountDirectory` (contas ricas, KYC, produtos), emite JWT, monta extrato como *read model* a partir dos eventos do ledger, e expõe um BFF ao app. O PixCore não conhece clientes, apenas `customer_id` opaco.

## Implantação
Um **tenant por instalação** (mais simples e seguro que multi-tenant). ISPB, limites e prazos vêm de configuração validada na subida (falha rápida se inválida).

## Compatibilidade e upgrade
- Semver por serviço e **matriz de compatibilidade** serviço × contrato ([ADR-0007](adr/0007-versioned-contracts-and-sdks.md)).
- Mudança breaking cria `v2` coexistindo com `v1`; janela de depreciação documentada.
- Guia de upgrade acompanha cada release com migrações de banco compatíveis (expand/contract).

## Licença e responsabilidade
Apache-2.0. Software financeiro: a responsabilidade regulatória e operacional é do adotante.
