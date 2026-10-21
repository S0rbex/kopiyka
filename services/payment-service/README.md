# payment-service

Менеджер, що веде платіж від створення до успіху чи відмови. Суть сервісу: [docs/services/payment-service.md](../../docs/services/payment-service.md).

## Шари (пакет `com.kopiyka.payment`)

| Пакет | Що тут живе | Від чого може залежати |
|---|---|---|
| `api` | REST-контролери, DTO запитів і відповідей | `application` |
| `application` | сценарії (use cases): «створити платіж», «провести проводку»; межі транзакцій | `domain` |
| `domain` | бізнес-правила й моделі: стани, `Money`, інваріанти. **Без Spring** | ні від чого |
| `infrastructure` | БД, Kafka, HTTP-клієнти до інших сервісів, конфігурація Spring | `application`, `domain` |

Правило «`domain` не залежить від Spring» перевірятиме ArchUnit (фаза 0).

## Що з'явиться у фазі 0

- `pom.xml` з батьківським `kopiyka-parent`
- `Application.java` і `application.yml`
- відповідь на `/actuator/health`

Інфраструктура: своя БД Postgres, Kafka (outbox, фаза 1).
