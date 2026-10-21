# 03. REST API

Публічний API призначений для серверів мерчантів і ходить через `api-gateway` (`http://localhost:8080`). Повний контракт — [public-api.yaml](../api/openapi/public-api.yaml). Тут — конвенції та пояснення, чому зроблено саме так.

## 1. Загальні конвенції

| Що | Як |
|---|---|
| Формат | JSON, поля в `snake_case`. Невідоме поле в запиті дає `400`: строгі до вхідних даних, толерантні до вихідних (клієнт має ігнорувати нові поля у відповідях) |
| Суми | ціле число в мінорних одиницях: `10050` = 100,50 грн. Валюта за ISO 4217: `UAH`, `USD`, `EUR` |
| Час | RFC 3339 в UTC: `2026-10-21T10:15:30Z` |
| ID | непрозорі рядки з префіксом типу: `pay_`, `re_`, `evt_`, `we_`, `whd_`, `txn_`, `mer_` |
| Тип об'єкта | кожен ресурс має поле `object`: `"payment"`, `"refund"`, `"list"`… |
| Метадані | `metadata` — до 20 пар рядок→рядок (ключ ≤ 40, значення ≤ 500 символів), напр. `order_id` магазину |
| Автентифікація | `Authorization: Bearer sk_test_…` |
| Трасування | у кожній відповіді є `Request-Id` (= trace id у Grafana). Вхідний `traceparent` підхоплюється |
| Версіонування | мажорна версія в шляху (`/v1`). Нові поля й нові типи подій — не breaking change. Breaking change означає `/v2`: у Spring Framework 7 є вбудоване API versioning, покажемо його у фазі 6 |

**Дії, які не вкладаються в CRUD, — окремі підресурси.** Пишемо `POST /v1/payments/{id}/confirm`, а не `PATCH {"status": "succeeded"}`: клієнт просить виконати дію, а стан визначає сервер.

## 2. Ідемпотентність

Кожен `POST`, що рухає гроші, вимагає заголовок `Idempotency-Key` (рекомендуємо UUID). Ключ діє в межах мерчанта 24 години.

| Ситуація | Відповідь |
|---|---|
| Перший запит з цим ключем | виконується; результат (2xx і 4xx) зберігається |
| Повтор з тим самим ключем і тим самим тілом | та сама відповідь + `Idempotent-Replayed: true` |
| Повтор, поки перший запит ще виконується | `409 idempotency_key_in_use` + `Retry-After: 1` |
| Той самий ключ, інше тіло | `422 idempotency_key_mismatch` |
| Перший запит завершився 5xx або сервер перезапустився | відповідь не зберігаємо; повтор **продовжує** операцію з місця зупинки |
| Ключа немає | `400 idempotency_key_required` |

Алгоритм — у [02-flows.md](02-flows.md), флоу 3. Заголовок описано в чернетці IETF «The Idempotency-Key HTTP Header Field».

## 3. Помилки: RFC 9457 Problem Details

```http
HTTP/1.1 409 Conflict
Content-Type: application/problem+json
Request-Id: 4bf92f3577b34da6a3ce929d0e0e4736

{
  "type": "https://api.kopiyka.example/errors/payment-unexpected-state",
  "title": "Payment is in an unexpected state",
  "status": 409,
  "detail": "Payment pay_7d1Lr0tRkX2b9QeVwYz4 is succeeded; confirm requires requires_confirmation.",
  "instance": "/v1/payments/pay_7d1Lr0tRkX2b9QeVwYz4/confirm",
  "code": "payment_unexpected_state",
  "request_id": "4bf92f3577b34da6a3ce929d0e0e4736"
}
```

Помилки валідації (`422`) додатково містять `errors: [{"field": "amount", "code": "min", "message": "must be >= 1"}]`. У Spring це `ProblemDetail` + `@RestControllerAdvice` з `platform-starter`.

| `code` | HTTP | Коли |
|---|---|---|
| `invalid_request` | 400 | JSON не парситься, невідоме поле, неправильний тип |
| `idempotency_key_required` | 400 | `POST` без ключа |
| `authentication_failed` | 401 | ключа немає, він невалідний або відкликаний |
| `permission_denied` | 403 | ключу бракує прав (restricted keys, фаза 6) |
| `resource_not_found` | 404 | ресурсу немає **або він чужий**: існування чужих ресурсів не розкриваємо |
| `payment_unexpected_state` | 409 | дія недоступна в поточному стані |
| `idempotency_key_in_use` | 409 | паралельний запит з тим самим ключем |
| `precondition_failed` | 412 | `If-Match` не збігся з поточною версією |
| `validation_failed` | 422 | поля не пройшли валідацію |
| `idempotency_key_mismatch` | 422 | той самий ключ з іншими параметрами |
| `amount_exceeds_capturable` | 422 | capture більший за авторизовану суму |
| `refund_exceeds_captured` | 422 | сума повернень перевищує списане |
| `precondition_required` | 428 | `PATCH` без `If-Match` |
| `rate_limited` | 429 | перевищено ліміт, є `Retry-After` |
| `bank_unavailable` | 503 | circuit breaker до банку відкритий, є `Retry-After` |
| `fraud_check_unavailable` | 503 | антифрод недоступний, а сума вища за поріг fail-open |
| `internal_error` | 500 | усе інше; деталі — тільки в логах за `request_id` |

**Відмова банку — не помилка API.** `confirm` з відхиленою карткою повертає `200` і платіж зі `status: "failed"` та `failure_code: "card_declined"`. Запит виконано коректно, просто результат негативний. Stripe у такому випадку повертає `402`, і на співбесіді варто вміти пояснити обидва варіанти.

## 4. Статус-коди

| Код | Де використовуємо |
|---|---|
| `200` | читання; дії, що завершилися синхронно |
| `201` + `Location` | ресурс створено (`POST /v1/payments`, `POST /v1/webhook_endpoints`) |
| `202` + `Location` + `Retry-After` | операцію прийнято, але вона ще не завершена (`confirm` при повільному банку, повернення) |
| `204` | `DELETE` |
| `304` | `If-None-Match` збігся з `ETag`: поллінг без тіла |
| `4xx`, `5xx` | див. таблицю помилок вище |

## 5. Пагінація курсором

```http
GET /v1/payments?limit=20&status=succeeded&created_from=2026-10-01T00:00:00Z
```

```json
{
  "object": "list",
  "data": [{ "id": "pay_7d1Lr0tRkX2b9QeVwYz4", "object": "payment" }],
  "has_more": true,
  "next_cursor": "eyJpZCI6IjAxOTJmNWM4In0"
}
```

Наступна сторінка: `?cursor=<next_cursor>`, решта фільтрів ті самі. Сортування — від нових до старих. Курсор непрозорий (base64url від останнього `id`), тому його формат можна змінити, не зламавши клієнтів.

**Чому не `offset`:**
- `OFFSET 100000` змушує БД прочитати й відкинути 100 000 рядків;
- нові записи між запитами зсувають сторінки, і клієнт бачить дублікати або пропуски.

Keyset-запит іде індексом одразу з потрібного місця (сам запит — у [05-data-model.md](05-data-model.md)).

## 6. Конкурентне редагування через ETag

Змінювані налаштування (webhook endpoints) захищені optimistic concurrency на рівні HTTP:

1. `GET /v1/webhook_endpoints/we_1` повертає `ETag: "7"` — версію рядка в БД.
2. Клієнт надсилає `PATCH` з `If-Match: "7"` і `Content-Type: application/merge-patch+json` (RFC 7396).
3. Якщо хтось устиг змінити ресурс раніше — `412 precondition_failed`. Без `If-Match` — `428 precondition_required`.

`GET` з `If-None-Match: "7"` повертає `304 Not Modified` без тіла. Так дешево поллити статус платежу.

ETag **сильний** (`"7"`, без `W/`): RFC 9110 вимагає для `If-Match` strong comparison, і слабкі теги там ніколи не збігаються.

## 7. Rate limiting

- **Ліміт на мерчанта:** token bucket у Redis (`RequestRateLimiter` у Spring Cloud Gateway), напр. 50 запитів/с з burst 100.
- **Заголовки у відповіді:** `X-RateLimit-Remaining`, `X-RateLimit-Replenish-Rate`, `X-RateLimit-Burst-Capacity`.
- **При перевищенні — `429` + `Retry-After`.** Gateway за замовчуванням віддає порожній 429, тому загортаємо його в Problem Details власним фільтром.
- **Якщо Redis недоступний, лімітер пропускає запити (fail-open).** Це свідомий вибір: краще тимчасово працювати без лімітів, ніж покласти API через лімітер.

## 8. Довгі операції

`confirm` чекає банк до ~3 с. Якщо результату немає, відповідь — `202 Accepted` зі `status: "processing"`, `Location` і `Retry-After`. Далі мерчант або отримає вебхук `payment.succeeded` / `payment.failed`, або поллить `GET` з `If-None-Match`.

## 9. Ендпоінти публічного API

| Метод | Шлях | Сервіс | `Idempotency-Key` | Фаза | Примітка |
|---|---|---|---|---|---|
| POST | `/v1/payments` | payment | обов'язковий | 1 | `201` + `Location` |
| GET | `/v1/payments` | payment | — | 1 | курсор; фільтри `status`, `created_from`, `created_to` |
| GET | `/v1/payments/{id}` | payment | — | 1 | `ETag`, `If-None-Match` → `304` |
| POST | `/v1/payments/{id}/confirm` | payment | обов'язковий | 1 | `200` (фінальний стан) або `202` (`processing`) |
| POST | `/v1/payments/{id}/capture` | payment | обов'язковий | 4 | manual capture, можна частково |
| POST | `/v1/payments/{id}/cancel` | payment | обов'язковий | 4 | скасування або void |
| POST | `/v1/refunds` | payment | обов'язковий | 4 | `202`, статус `pending` |
| GET | `/v1/refunds`, `/v1/refunds/{id}` | payment | — | 4 | фільтр `payment_id` |
| GET | `/v1/balance` | ledger | — | 1 | `available`, `pending`, `reserved` по валютах |
| GET | `/v1/balance_transactions` | ledger | — | 1 | рух коштів, курсор |
| GET | `/v1/events`, `/v1/events/{id}` | webhook | — | 3 | журнал подій за 30 днів |
| POST | `/v1/webhook_endpoints` | webhook | обов'язковий | 3 | `secret` повертається один раз |
| GET | `/v1/webhook_endpoints`, `/v1/webhook_endpoints/{id}` | webhook | — | 3 | `ETag` |
| PATCH | `/v1/webhook_endpoints/{id}` | webhook | — | 3 | JSON Merge Patch, `If-Match` обов'язковий |
| DELETE | `/v1/webhook_endpoints/{id}` | webhook | — | 3 | `204` |
| POST | `/v1/webhook_endpoints/{id}/rotate_secret` | webhook | обов'язковий | 3 | старий секрет діє ще 24 год |
| GET | `/v1/webhook_endpoints/{id}/deliveries` | webhook | — | 3 | журнал доставок, фільтр `status` |
| POST | `/v1/webhook_deliveries/{id}/retry` | webhook | обов'язковий | 3 | `202` |
| GET | `/v1/account` | merchant | — | 1 | профіль і тариф |

## 10. Приклади

### Створити платіж

```http
POST /v1/payments
Authorization: Bearer sk_test_9Qm2…
Idempotency-Key: 6f1c2a1e-1b0a-4f7e-9a51-7c1a4b1f8e21
Content-Type: application/json

{ "amount": 10050, "currency": "UAH", "description": "Замовлення №1001", "metadata": { "order_id": "1001" } }
```

```http
HTTP/1.1 201 Created
Location: /v1/payments/pay_7d1Lr0tRkX2b9QeVwYz4
ETag: "0"

{ "id": "pay_7d1Lr0tRkX2b9QeVwYz4", "object": "payment", "amount": 10050, "currency": "UAH",
  "status": "requires_confirmation", "capture_method": "automatic", "...": "..." }
```

### Підтвердити платіж

```http
POST /v1/payments/pay_7d1Lr0tRkX2b9QeVwYz4/confirm
Idempotency-Key: 0b9e7c4d-2f61-4c1a-8e0f-5d3b9a7c6e12
Content-Type: application/json

{ "payment_method": { "type": "card",
    "card": { "number": "4242424242424242", "exp_month": 12, "exp_year": 2028, "cvc": "123" } } }
```

Банк відповів швидко:

```http
HTTP/1.1 200 OK
ETag: "3"

{
  "id": "pay_7d1Lr0tRkX2b9QeVwYz4",
  "object": "payment",
  "amount": 10050,
  "currency": "UAH",
  "status": "succeeded",
  "capture_method": "automatic",
  "amount_capturable": 0,
  "amount_captured": 10050,
  "amount_refunded": 0,
  "fee": 251,
  "net": 9799,
  "card": { "brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2028 },
  "risk_level": "normal",
  "failure_code": null,
  "failure_message": null,
  "description": "Замовлення №1001",
  "metadata": { "order_id": "1001" },
  "capture_before": null,
  "created_at": "2026-10-21T10:15:28Z",
  "updated_at": "2026-10-21T10:15:30Z"
}
```

Банк не встиг відповісти:

```http
HTTP/1.1 202 Accepted
Location: /v1/payments/pay_7d1Lr0tRkX2b9QeVwYz4
Retry-After: 5
ETag: "1"

{ "id": "pay_7d1Lr0tRkX2b9QeVwYz4", "object": "payment", "status": "processing", "...": "..." }
```

Комісія: `10050 × 250 bps / 10 000 = 251,25` → `HALF_UP` → `251`. Отже, `net = 9799`.

### Повернення

```http
POST /v1/refunds
Idempotency-Key: 3c8d1f0a-6b2e-4d7f-9a1c-8e5b2d4f7a90
Content-Type: application/json

{ "payment_id": "pay_7d1Lr0tRkX2b9QeVwYz4", "amount": 6000, "reason": "requested_by_customer" }
```

```http
HTTP/1.1 202 Accepted
Location: /v1/refunds/re_4Tq9Zk2Lm8Pw

{ "id": "re_4Tq9Zk2Lm8Pw", "object": "refund", "payment_id": "pay_7d1Lr0tRkX2b9QeVwYz4",
  "amount": 6000, "currency": "UAH", "status": "pending", "reason": "requested_by_customer",
  "failure_code": null, "metadata": {}, "created_at": "2026-10-21T11:02:10Z", "updated_at": "2026-10-21T11:02:10Z" }
```

### Вебхук

```http
POST https://shop.example/webhooks/kopiyka
Content-Type: application/json
Kopiyka-Event-Id: evt_3f9kQ2Wd7Lx
Kopiyka-Signature: t=1792577730,v1=9f2a1c7be04d5e83a6c1f0b2d4e7a9c3b5d8e1f4a7c0b3d6e9f2a5c8b1d4e7f0

{ "id": "evt_3f9kQ2Wd7Lx", "object": "event", "type": "payment.succeeded", "created_at": "2026-10-21T10:15:30Z",
  "data": { "object": { "id": "pay_7d1Lr0tRkX2b9QeVwYz4", "object": "payment", "status": "succeeded", "...": "..." } } }
```

Перевірка на боці мерчанта:
- `v1 == hex(HMAC_SHA256(secret, t + "." + raw_body))`, порівняння constant-time;
- `|now − t| ≤ 5 хв` — захист від replay;
- дедуплікація за `Kopiyka-Event-Id`.

Підпис рахується від **сирого** тіла. Якщо розпарсити JSON і серіалізувати його заново, байти зміняться і підпис не зійдеться.

## 11. Внутрішні API

Доступні тільки всередині docker-мережі, gateway їх не маршрутизує. У фазі 6 вимагають JWT з Keycloak.

| Сервіс | Метод і шлях | Хто викликає | Таймаут і ретраї |
|---|---|---|---|
| merchant | `POST /internal/v1/api-keys/verify` | gateway | 300 мс, кеш 60 с |
| merchant | `GET /internal/v1/merchants/{id}` | payment (тариф) | 300 мс, кеш 5 хв (до фази 5) |
| fraud | `POST /internal/v1/assessments` | payment | 300 мс, без ретраїв, CB + fallback |
| ledger | `POST /internal/v1/holds` | payment (повернення) | 1 с, ретрай з тим самим `reference` |
| ledger | `GET /internal/v1/holds/{id}` | payment | 1 с |
| fx | `GET /internal/v1/rates/{base}/{quote}` | fraud (до появи compacted-топіка) | 300 мс |
| bank | `POST /bank/v1/…` | payment | connect 1 с, read 3 с; ретрай тільки з тим самим `Idempotency-Key` |
| bank | `GET /bank/v1/settlement-reports/{date}` | settlement | 30 с |

**Admin API** (`/admin/v1/**`, Keycloak-ролі `admin` і `support`, фаза 6):
- створення мерчантів;
- випуск і відкликання ключів;
- перегляд розбіжностей reconciliation;
- ручне вирішення платежів у `manual_review`.

До фази 6 мерчанти й ключі заводяться seed-міграцією Flyway.

**Приклад відповіді fraud-service:**

```json
{ "decision": "allow", "score": 12, "reasons": ["velocity_ok", "amount_typical"], "model_version": "2026-10-01" }
```

## 12. bank-simulator

Контракт — [bank-simulator.yaml](../api/openapi/bank-simulator.yaml). Кожен `POST` вимагає `Idempotency-Key`. Автентифікація — заголовок `X-Bank-Api-Key`.

Поведінку задають тестові картки. Перші чотири взято з тестових карток Stripe, решту придумано для наших сценаріїв. Усі номери проходять перевірку Luhn.

| Номер | Поведінка |
|---|---|
| `4242 4242 4242 4242` | успіх |
| `4000 0000 0000 0002` | відмова `card_declined` |
| `4000 0000 0000 9995` | відмова `insufficient_funds` |
| `4000 0000 0000 0069` | відмова `expired_card` |
| `4000 0000 0000 0119` | `500`; операція записана як `failed`, inquiry покаже `failed` |
| `4000 0000 0000 0077` | відповідь через 10 с: клієнт ловить таймаут, а банк операцію проводить |
| `4000 0000 0000 0085` | ghost: операцію проведено, з'єднання обірвано без відповіді |
| `4000 0000 0000 0093` | authorize проходить, capture відхиляється (перевіряємо компенсацію через void) |

**Хаос задається на рівні всього банку:**

```http
PUT /bank/admin/chaos
Content-Type: application/json

{ "latency_ms": { "p50": 120, "p99": 900 }, "error_rate": 0.02, "timeout_rate": 0.01, "ghost_rate": 0.005 }
```

- `error_rate` — `500` **до** обробки: операцію не записано, inquiry поверне `404`, повтор безпечний.
- `timeout_rate` — операція проводиться після затримки, довшої за таймаут клієнта.
- `ghost_rate` — операцію проведено, відповідь втрачено.

## 13. Від контракту до коду

- **Java:** openapi-generator (генератор `spring`, режим `interfaceOnly`). Контролери реалізують згенеровані інтерфейси, DTO теж згенеровані. Клієнти — `@HttpExchange`-інтерфейси або згенеровані.
- **Go:** oapi-codegen генерує сервер bank-simulator з того самого YAML. Один контракт, дві мови — і розбіжність ловиться ще на етапі компіляції.
- **TypeScript:** openapi-typescript + openapi-fetch у merchant-demo.
- **CI:** Spectral лінтить стиль API, oasdiff блокує PR з breaking changes.
