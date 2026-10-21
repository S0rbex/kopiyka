# 06. Поетапний план від MVP

Оцінки орієнтовні, з розрахунку на 10–15 годин на тиждень. Разом виходить ≈ 18–22 тижні.

**Показувати проєкт на співбесідах можна вже після фази 2 (≈ 2 місяці).** На той момент є ідемпотентність, сага з таймаутами банку, outbox і double-entry ledger — це ядро розповіді. Кожна наступна фаза додає нову тему.

## Правила гри

1. **Вертикальні зрізи.** Кожна фаза закінчується робочим сценарієм end-to-end, а не «написав половину кожного сервісу».
2. **Спершу тест на інваріант, потім код.** Наприклад: «10 паралельних confirm → рівно один успішний», «сума проводок = 0».
3. **Кожне неочевидне рішення — ADR** у `docs/adr/`, 5–10 речень.
4. **Не переходиш до наступної фази, поки не виконано її Definition of Done.**
5. **Маленькі осмислені коміти, кожна фаза — PR з описом**, навіть у власному репозиторії. Так виглядає робота на реальному проєкті.
6. **Код, згенерований AI, — тільки якщо можеш пояснити кожен рядок.** На співбесіді копатимуть саме там, де ти не впевнений.

## Фаза 0 — Фундамент (≈ 1 тиждень)

**Мета:** порожні сервіси збираються, проходять CI і піднімаються однією командою.

- [ ] Монорепо: Maven multi-module (батьківський `pom.xml` на базі `spring-boot-starter-parent`), Java 25, Spring Boot 4. Поруч — Go-модуль bank-simulator. Тільки сервіси фази 1: gateway, merchant, payment, ledger, bank-simulator; решта додаються у своїх фазах
- [ ] `libs/platform-starter`:
  - [ ] Problem Details і `Request-Id`
  - [ ] Jackson `snake_case` + строгий парсинг запитів
  - [ ] бін `Clock`, щоб керувати часом у тестах
  - [ ] базовий клас для Testcontainers
- [ ] `deploy/docker-compose.yml`, профіль `infra`: Postgres 18 (окрема БД і користувач на кожен сервіс), Kafka 4 у KRaft з `auto.create.topics.enable=false`, Redis, Kafka UI, `grafana/otel-lgtm`
- [ ] Трейси й метрики Java-сервісів одразу йдуть в otel-lgtm
- [ ] GitHub Actions: build, тести, Spotless. ArchUnit-правило «`domain` не залежить від Spring»
- [ ] ADR-0001 «мікросервіси свідомо, для навчання», ADR-0002 «гроші в мінорних одиницях», ADR-0003 «монорепо»

**Done:**
- CI зелений;
- `docker compose --profile infra up` піднімає інфраструктуру;
- кожен сервіс відповідає на `/actuator/health`.

## Фаза 1 — MVP: платіж проходить і потрапляє в ledger (≈ 4–5 тижнів)

**Мета:** мерчант створює й підтверджує платіж тестовою карткою, і гроші з'являються на балансі. Сервіси: payment, bank-simulator, ledger, merchant, gateway.

Порядок роботи (кожен крок дає робочий результат):

1. **bank-simulator (Go):** authorize, capture, void і inquiry, тестові картки, `Idempotency-Key`. Сервер генерується з [bank-simulator.yaml](../api/openapi/bank-simulator.yaml) через oapi-codegen.
2. **payment-service:** `POST/GET /v1/payments` з ідемпотентністю. Поки що мерчант береться просто із заголовка `X-Merchant-Id`.
3. **confirm:**
   - fraud — заглушка, що завжди відповідає «allow»;
   - умовні переходи стану;
   - `bank_operations` з write-ahead intent;
   - комісія з конфігурації.
4. **Outbox (polling) → `payments.events.v1`:** JSON + заголовки `ce_id`/`ce_type`/`traceparent`, один активний паблішер через ShedLock.
5. **ledger-service:**
   - рахунки й проводка `payment.succeeded`, ідемпотентна за `external_ref`;
   - блокування рахунків у фіксованому порядку;
   - `GET /v1/balance`, `GET /v1/balance_transactions` з курсором.
6. **merchant-service + api-gateway:**
   - API-ключі (SHA-256), перевірка в gateway з кешем у Redis;
   - очищення вхідних `X-Merchant-*`, rate limiting;
   - тариф для payment — з merchant-service (REST + кеш), `GET /v1/account`.
7. **Тести:**
   - unit: стейт-машина, `Money`, комісія;
   - інтеграційні з Testcontainers;
   - конкурентні: 10 паралельних confirm → рівно 1 успіх; 10 паралельних запитів з одним ключем → 1 виконання, решта отримують `409` або replay.

**Done:** сценарій у `http/mvp.http` проходить повністю:
- create → confirm карткою 4242 → `succeeded` → баланс зріс на 97,99 грн;
- повторний confirm з тим самим ключем повертає ту саму відповідь;
- картка 0002 дає `failed`;
- trial balance = 0.

**Що розповідатимеш:** ідемпотентність, `@Transactional` і межі транзакцій, умовні апдейти проти `@Version`, double-entry, outbox, keyset-пагінація.

## Фаза 2 — Надійність (≈ 3 тижні)

**Мета:** система переживає таймаути банку, падіння сервісів і дублікати, не втрачаючи грошей.

- [ ] Resilience4j на клієнті банку: connect 1 с, read 3 с, ретраї тільки з тим самим `Idempotency-Key`, circuit breaker. На fraud — 300 мс без ретраїв
- [ ] Статус `unknown`: `202 processing`, `InquiryJob` (claim + lease, backoff від 5 с до 24 год), відновлення операцій, що зависли в `pending` після падіння
- [ ] Хаос у bank-simulator (`PUT /bank/admin/chaos`) + Toxiproxy в інтеграційних тестах (затримка, обрив з'єднання). OpenTelemetry у Go: затримки банку видно в трейсі
- [ ] Debezium CDC замість polling outbox (Kafka Connect, `wal_level=logical`), `traceparent` через outbox
- [ ] Inbox у ledger, blocking retry з `DefaultErrorHandler`, `ErrorHandlingDeserializer` + DLT для poison pill
- [ ] Перевірка інваріантів після chaos-прогону:
  - [ ] кожен `succeeded` платіж має рівно одну проводку capture
  - [ ] немає платежів у `processing` довше 5 хв
  - [ ] trial balance = 0

**Done:** при `ghost_rate=0.05`, `timeout_rate=0.05` і перезапуску payment-service посеред навантаження всі інваріанти виконуються.

**Що розповідатимеш:** таймаут ≠ відмова, write-ahead intent, circuit breaker + retry + jitter, dual write і outbox через CDC, at-least-once + ідемпотентний консюмер, чому для ledger не підходять retry topics.

## Фаза 3 — Контракти й вебхуки (≈ 3 тижні)

- [ ] Schema Registry + Avro для `payments.events.v1`. Переходимо до появи нових консюмерів, бо так дешевше. Перевірка сумісності схем у CI
- [ ] webhook-service:
  - [ ] endpoints з `ETag`/`If-Match` і JSON Merge Patch
  - [ ] публічні події `GET /v1/events`
  - [ ] доставки з HMAC-підписом через чергу в БД (`SKIP LOCKED` + lease + fencing), backoff ≈ 3 доби
  - [ ] ручний retry, ротація секрету, захист від SSRF
- [ ] merchant-demo (TypeScript): створює платежі через згенерований клієнт, приймає вебхуки, перевіряє підпис і навмисно іноді відповідає `500`
- [ ] Contract testing: Pact, де consumer — merchant-demo, а provider — публічний API
- [ ] Spectral і oasdiff у CI

**Done:**
- merchant-demo отримує `payment.succeeded`;
- якщо merchant-demo вимкнений, доставки ретраяться й доходять після його старту;
- вебхук з невалідним підписом відкидається.

**Що розповідатимеш:** еволюція схем і режими сумісності, вебхуки з at-least-once, HMAC і захист від replay, SSRF, чому черга ретраїв у БД, consumer-driven contracts між різними мовами.

## Фаза 4 — Повна платіжна модель (≈ 3–4 тижні)

- [ ] Manual capture: `requires_capture`, часткове списання, `cancel`/void, автоскасування за `capture_before`
- [ ] Повернення:
  - [ ] умовний резерв на платежі
  - [ ] hold у ledger
  - [ ] refund у банку
  - [ ] завершення через `refund.succeeded` / `refund.failed`
- [ ] fraud-service (Python):
  - [ ] правила, velocity-лічильники в Redis і ML-модель на синтетичних даних
  - [ ] фічі з `payments.events.v1`
  - [ ] fallback-політика в payment-service
- [ ] fx-service *(опційно)*: Monobank → НБУ → останній відомий курс, compacted-топік `fx.rates.v1` для fraud-service
- [ ] OpenTelemetry у Python: один трейс проходить через Java, Python і Go

**Done:**
- часткові повернення 60 + 60 на платіж 100: друге відхилено;
- банк відмовив у поверненні → hold звільнено;
- fraud-service вимкнено → платежі до порогу проходять.

**Що розповідатимеш:** саги з компенсаціями й pivot, гонки на межі часу, fail-open проти fail-closed, polyglot-мікросервіси.

## Фаза 5 — Гроші сходяться (≈ 2–3 тижні)

- [ ] settlement-service: Spring Batch job (звіт банку → staging → зіставлення з проєкцією з подій → розбіжності), рестарт з місця падіння, ShedLock
- [ ] Ledger: capture тепер іде в `pending`, а в `available` гроші переносить `settlement.completed`
- [ ] Політика для ghost-операцій, які знайшла звірка: автоматичний refund + алерт
- [ ] Публічна подія `balance.available`
- [ ] ECST: compacted-топік `merchants.state.v1` замість синхронного виклику merchant-service з payment-service
- [ ] *(опційно)* виплати мерчантам: `POST /v1/payouts`

**Done:** після дня з chaos-навантаженням звірка не знаходить жодної нез'ясованої розбіжності. Або кожна розбіжність має пояснення й автоматичну дію.

**Що розповідатимеш:** reconciliation як остання лінія оборони, batch-обробка й рестартабельність, проєкції з подій замість join між сервісами, log compaction.

## Фаза 6 — Production-ready (≈ 2–3 тижні)

- [ ] Keycloak:
  - [ ] client credentials між сервісами, scope на кожну внутрішню операцію
  - [ ] ролі для admin API
  - [ ] інвалідація кешу ключів у gateway через `merchants.events.v1`
- [ ] Grafana: дашборди Payments / Bank / Kafka / Webhooks / Ledger і алерти з [01-architecture.md](01-architecture.md)
- [ ] k6:
  - [ ] сценарії: 100 RPS confirm, burst до `429`, «шторм» однакових ключів
  - [ ] знайти й прибрати вузьке місце (очікуваний кандидат — hot account `platform:fees` → шардування на субрахунки)
  - [ ] результат «до / після» в README
- [ ] Показати вбудоване API versioning Spring Framework 7 на одній зміні
- [ ] *(stretch)* k3d + Helm, 3 брокери Kafka з `min.insync.replicas=2`, analytics на Kafka Streams з `exactly_once_v2`
- [ ] README англійською, діаграми, коротке демо-відео

**Done:** людина, яка бачить проєкт уперше, за 10 хвилин розуміє його з README і запускає демо однією командою.

## Чого не робити (анти-скоуп)

- Не пиши UI-дашборд: Swagger UI, `.http`-файли, merchant-demo і Grafana закривають усе.
- Не пиши власний auth-сервер: для цього є Keycloak.
- Не берися за Kubernetes до фази 6: він з'їсть тижні й не додасть розуміння грошових флоу.
- Не додавай сервісів понад список. Глибина важливіша за кількість.
- Не оптимізуй продуктивність, поки k6 не показав реальне вузьке місце.

## 5-хвилинне демо на співбесіді

1. `docker compose up`, відкриваєш Grafana.
2. Створюєш і підтверджуєш платіж карткою 4242. У трейсі видно gateway → payment → fraud → bank, у ledger — три проводки.
3. Повторюєш той самий запит: приходить `Idempotent-Replayed: true`.
4. Картка 0085 (ghost): `202 processing` → inquiry з'ясовує результат → вебхук приходить у merchant-demo.
5. Зупиняєш ledger-service: платежі продовжують проходити. Запускаєш знову: lag розсмоктується, баланс правильний.
6. Два паралельні повернення: одне з них отримує `422`.
7. Наостанок — trial balance = 0 і чистий звіт reconciliation.

## Типові питання на співбесіді → де це в проєкті

| Питання | Де | Фаза |
|---|---|---|
| Як зробити `POST` ідемпотентним? | `Idempotency-Key` у payment-service | 1 |
| `@Transactional`: межі, propagation, self-invocation | confirm: кілька коротких транзакцій навколо викликів банку | 1 |
| Optimistic vs pessimistic locking | умовні переходи стану vs `FOR UPDATE` у ledger | 1 |
| Рівні ізоляції, lost update | рахунки ledger, резерв під повернення | 1, 4 |
| Deadlock: як отримати і як уникнути | зустрічні проводки, фіксований порядок блокувань | 1 |
| N+1 і коли не JPA | списки платежів; `JdbcClient` для ledger і `SKIP LOCKED` | 1 |
| Розподілені транзакції, Saga | confirm і refund, pivot, компенсації | 1, 4 |
| Dual write і Outbox | polling → Debezium | 1, 2 |
| Гарантії доставки Kafka, exactly-once | ідемпотентні консюмери, inbox | 2 |
| Порядок повідомлень у Kafka | ключ = `payment_id`, один топік для платежів і повернень | 1 |
| Що робити з poison pill | `ErrorHandlingDeserializer` + DLT | 2 |
| Circuit breaker, retry, timeout | клієнти bank і fraud | 2 |
| Еволюція схем подій | Avro + Schema Registry | 3 |
| Безпека API | API-ключі, HMAC вебхуків, SSRF, JWT між сервісами | 1, 3, 6 |
| Пагінація великих списків | keyset по UUIDv7 | 1 |
| Як шукати проблему в мікросервісах | трейс за `Request-Id` у Grafana | 0–6 |
| Як тестуєте інтеграції | Testcontainers, Toxiproxy, Pact | 1–3 |
| Навантажувальне тестування | k6, hot account | 6 |
