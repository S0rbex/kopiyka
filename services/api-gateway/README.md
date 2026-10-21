# api-gateway

Єдина вхідна точка: маршрутизація, перевірка API-ключів, rate limiting. Суть сервісу: [docs/services/api-gateway.md](../../docs/services/api-gateway.md).

## Шари (пакет `com.kopiyka.gateway`)

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

Інфраструктура: Redis (кеш ключів, rate limiting). **Увага:** Spring Cloud Gateway реактивний (WebFlux), тому залежності тут інші, ніж у решти сервісів. Власної БД немає.
