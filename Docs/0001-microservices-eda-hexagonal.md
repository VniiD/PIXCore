# ADR-0001: Microsserviços com EDA e hexagonal por serviço

- **Status:** Proposto (substitui a proposta anterior de monólito modular)
- **Data:** 2026-11-02

## Contexto
O PixCore é uma infraestrutura *bank-in-a-box* para ser adotada em outros projetos, sendo o banco digital (repositório separado) o primeiro consumidor. Os requisitos incluem isolar o ledger (maior criticidade), isolar a fronteira regulatória (certificados, rede do regulador) e usar EDA, padrão em fintechs e bancos digitais.

## Decisão
Seis deployables, cada um com **dono de dados** e **banco próprio** (PostgreSQL). Linguagem: **Go**.

| Serviço | Dono dos dados | Por que separado |
|---|---|---|
| `account-service` | contas (registro), chaves Pix | Implementação de referência da porta `AccountDirectory`; ciclo de vida de conta e PII (LGPD) distintos do dinheiro |
| `ledger-service` | saldos, journals, entries, holds | Único escritor do dinheiro; maior criticidade; sem conceitos de Pix |
| `payment-service` | pagamentos, Saga, limites Pix e consumo, idempotência da API | Regra de negócio Pix e orquestração ([ADR-0010](0010-ownership-and-boundaries.md)) |
| `spi-gateway` | mensagens regulatórias | Segredos/certificados, rede regulatória, blast radius |
| `audit-notification-service` | trilha de auditoria, webhooks | Consumidor puro: não afeta o caminho do dinheiro; egress para internet isolado |
| `spi-simulator` | nenhum | Dev, CI e caos |

**Regras:**
1. Um dono por dado. Sem FK nem SQL entre bancos. Credenciais de banco distintas por serviço.
2. Comunicação **assíncrona por comandos e eventos** (broker, [ADR-0002](0002-broker-kafka.md)). gRPC síncrono só quando a resposta imediata é requisito: recebimento do SPI (`Credit`), leituras e autorização.
3. Hexagonal por serviço (`domain`, `app`, `ports`, `adapters`).
4. Nenhum serviço importa o domínio de outro. O único código compartilhado é `libs/platform` (logging, OTel, outbox/inbox, config, erros).
5. Edge (JWT, rate limit, roteamento) com produto pronto (Envoy ou Traefik), sem código próprio.
6. **Identidade fora do PixCore.** O token é emitido pelo consumidor (o banco). O edge valida o JWT e repassa `customer_id` e permissões como claims.

## Alternativas
| Alternativa | Observação |
|---|---|
| Monólito modular | Válido e mais simples de operar; preferível para time muito pequeno sem necessidade de EDA. Descartado porque o objetivo inclui EDA e o isolamento de ledger e regulador |
| Muitos serviços finos (10+) | Custo operacional e de consistência sem ganho proporcional |

## Consequências
- (+) Isolamento de falhas e segredos, escala independente, ledger sem acoplamento a Pix.
- (−) Consistência eventual entre serviços exige Saga ([ADR-0006](0006-saga-tcc.md)); tracing distribuído e testes de contrato são obrigatórios desde o dia 1.
- **Gatilho de revisão:** se o custo operacional dos serviços exceder a capacidade do time, consolidar `account-service` e `payment-service` mantendo as portas.
