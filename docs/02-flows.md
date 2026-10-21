# 02. Ключові флоу

Тут описано, як рухаються дані й гроші, що відбувається при збоях і чому система від цього не ламається. SQL-запити, на які є посилання, наведено в [05-data-model.md](05-data-model.md).

## Стейт-машина платежу

```mermaid
stateDiagram-v2
    [*] --> requires_confirmation: POST /v1/payments
    requires_confirmation --> processing: confirm
    requires_confirmation --> canceled: cancel
    processing --> requires_confirmation: fraud або банк недоступні, до виклику банку
    processing --> requires_capture: authorize ок, manual capture
    processing --> succeeded: authorize ок і capture ок
    processing --> failed: відмова банку, fraud block або capture ✗ з void
    requires_capture --> processing: capture або cancel
    processing --> canceled: void ок
    succeeded --> [*]
    failed --> [*]
    canceled --> [*]
```

- **Кожен перехід — умовний `UPDATE`:** `… WHERE id = ? AND status = <очікуваний>`. Якщо оновлено 0 рядків, хтось нас випередив. Мерчант у такому разі отримує `409 payment_unexpected_state`, а фонова задача просто пропускає рядок.
- **Внутрішній крок саги** зберігається в колонці `step`: `fraud_check` → `authorizing` → `capturing` | `voiding` | `awaiting_inquiry`. Назовні його не видно.
- **`succeeded`, `failed` і `canceled` — фінальні стани.** Повернення не змінюють статус платежу, тільки `amount_refunded`.

## Флоу 1. Створення й підтвердження платежу (automatic capture)

```mermaid
sequenceDiagram
    autonumber
    participant M as Мерчант
    participant GW as api-gateway
    participant P as payment-service
    participant DB as payment_db
    participant F as fraud-service
    participant B as bank-simulator
    participant R as outbox relay
    participant K as Kafka
    participant L as ledger-service
    participant W as webhook-service

    M->>GW: POST /v1/payments, Idempotency-Key k1
    GW->>GW: API-ключ → merchant_id, rate limit
    GW->>P: POST /v1/payments + X-Merchant-Id
    P->>DB: одна tx — ключ k1, payment requires_confirmation, outbox payment.created, відповідь
    P-->>M: 201 Created + Location

    M->>GW: POST /v1/payments/pay_1/confirm, Idempotency-Key k2
    GW->>P: confirm
    P->>DB: tx — ключ k2 in_progress, UPDATE status=processing WHERE status=requires_confirmation
    P->>F: POST /internal/v1/assessments, таймаут 300 мс
    F-->>P: allow, score 12
    P->>DB: tx — bank_operation authorize pending, ключ pay_1:authorize
    P->>B: POST /bank/v1/authorizations, Idempotency-Key pay_1:authorize
    B-->>P: 201 approved, auth_77
    P->>DB: tx — authorize approved, bank_operation capture pending
    P->>B: POST /bank/v1/authorizations/auth_77/capture, Idempotency-Key pay_1:capture
    B-->>P: 201 captured
    P->>DB: tx — payment succeeded, fee 251, outbox payment.succeeded, ключ k2 completed
    P-->>M: 200 OK, status succeeded
    R->>DB: читає outbox (polling або WAL)
    R->>K: payments.events.v1, key pay_1
    K-->>L: payment.succeeded → проводка payment:pay_1:capture
    K-->>W: payment.succeeded → evt_… → доставки
    W->>M: POST вебхук + Kopiyka-Signature
```

Що тут важливо:

1. **Створення платежу — одна транзакція.** Сам платіж, подія в outbox і збережена відповідь для ідемпотентності комітяться атомарно.
2. **Confirm — серія коротких транзакцій, між якими йдуть HTTP-виклики.** Жодна транзакція не лишається відкритою, поки чекаємо банк чи fraud: інакше з'єднання з пулу зайняті секундами, а рядки заблоковані.
3. **Пастка `@Transactional`.** Якщо розбити confirm на методи одного класу і викликати їх через `this`, транзакцій не буде: виклик іде повз Spring-проксі. Тому використовуємо `TransactionTemplate` або окремі біни.
4. **Write-ahead intent.** Операцію з банком записуємо в статусі `pending` і комітимо **до** HTTP-виклику. Якщо сервіс упаде посеред виклику, recovery-job знайде `pending` і з'ясує результат.
5. **Ключі ідемпотентності для банку детерміновані** (`pay_1:authorize`, `pay_1:capture`). Будь-який повтор безпечний: банк поверне той самий результат.
6. **Pivot — це capture.** Після нього рух тільки вперед: ledger і webhook-service отримують факт через outbox і Kafka, payment на них не чекає.

**Рішення fraud-service:**
- `block` → платіж `failed` з `fraud_blocked`.
- `review` → продовжуємо з `risk_level: elevated`.
- Fraud-service недоступний (таймаут або відкритий circuit breaker):
  - сума ≤ 5 000 грн-еквівалента → продовжуємо з `risk_level: elevated` (fail-open);
  - сума більша → платіж повертається в `requires_confirmation`, а мерчант отримує `503 fraud_check_unavailable` (fail-closed).

## Флоу 2. Таймаут банку → `unknown` → inquiry

Таймаут — **не** відмова: банк міг провести операцію і просто не встигнути відповісти.

```mermaid
sequenceDiagram
    autonumber
    participant M as Мерчант
    participant P as payment-service
    participant B as bank-simulator
    participant J as InquiryJob

    M->>P: POST /v1/payments/pay_2/confirm
    P->>B: POST /bank/v1/authorizations, ключ pay_2:authorize
    Note over B: операцію проведено,<br/>відповідь загубилась (ghost)
    B--xP: read timeout 3 с
    P->>P: bank_operation unknown, next_inquiry_at = now + 5 с
    P-->>M: 202 Accepted, status processing, Retry-After 5
    loop backoff 5 с → 15 с → 1 хв → 5 хв → … до 24 год
        J->>B: GET /bank/v1/operations/pay_2:authorize
        alt 200 approved або declined
            B-->>J: результат
            J->>J: продовжити сагу або завершити failed
        else 200 processing
            B-->>J: банк ще обробляє
            J->>J: наступна спроба пізніше
        else 404
            B-->>J: банк не бачив запит
            J->>B: повторити POST з тим самим ключем
        else банк недоступний
            J->>J: next_inquiry_at += backoff
        end
    end
    Note over J: через 24 год без відповіді — manual_review,<br/>остаточне рішення за reconciliation
```

Мерчант дізнається результат з вебхука або поллить `GET /v1/payments/pay_2` з `If-None-Match`. Поки статус не змінився, відповідь — `304`.

Той самий механізм підбирає операції, що «застрягли» в `pending` після падіння сервісу: recovery-job вважає `pending` старший за 30 с таким самим `unknown`.

## Флоу 3. Повтор запиту з тим самим Idempotency-Key

```text
POST з Idempotency-Key K від мерчанта M:
  hash := sha256(method + path + канонічний JSON тіла)   -- ключі відсортовані, без пробілів
  INSERT INTO idempotency_keys (M, K, hash, 'in_progress', locked_until = now() + 30 s)
      ON CONFLICT DO NOTHING
  якщо рядок вставлено → виконати операцію:
      успіх або бізнес-помилка 4xx  → зберегти статус і тіло відповіді, status = 'completed'
      5xx, і точно нічого не змінилось → DELETE ключа: повтор почнеться з нуля
      процес упав                    → нічого: lease закінчиться сам
  інакше → SELECT … FOR UPDATE:
      hash інший                     → 422 idempotency_key_mismatch
      completed                      → збережена відповідь + Idempotent-Replayed: true
      in_progress, lease ще діє      → 409 idempotency_key_in_use + Retry-After: 1
      in_progress, lease минув       → забрати lease і продовжити операцію з її збереженого стану
```

Останній рядок працює тому, що операцію можна продовжити з того місця, де вона зупинилась. Стан саги лежить у рядку платежу, а ключі для банку детерміновані. Це ідея «recovery points» зі статті Brandur Leach про ідемпотентність у Stripe. Сам Stripe зберігає навіть відповіді 5xx, ми свідомо ні.

## Флоу 4. Manual capture і автоскасування

1. **`capture_method: manual`.** Після authorize платіж переходить у `requires_capture` з `amount_capturable = amount` і `capture_before = now + 7 днів` (у dev — 10 хв).
2. **`POST /capture`** з `amount_to_capture ≤ amount_capturable`. Умовний перехід `requires_capture → processing`, capture у банку, далі `succeeded`. Несписаний залишок банк звільняє сам.
3. **`POST /cancel`.** Перехід `requires_capture → processing` (крок `voiding`), void у банку, далі `canceled`.
4. **`CaptureExpiryJob`** раз на хвилину бере прострочені платежі (`status = 'requires_capture' AND capture_before < now()`) через `FOR UPDATE SKIP LOCKED` і робить з ними те саме, що cancel. Причина скасування — `expired`.

**Гонка:** мерчант робить capture в ту саму мить, коли job робить void. Обидва виконують `UPDATE … WHERE status = 'requires_capture'`, і виграє рівно один. Другий отримує `409`, або job просто пропускає рядок.

## Флоу 5. Повернення: сага з холдом і компенсацією

```mermaid
sequenceDiagram
    autonumber
    participant M as Мерчант
    participant P as payment-service
    participant L as ledger-service
    participant B as bank-simulator
    participant K as Kafka

    M->>P: POST /v1/refunds, payment_id, amount 6000, Idempotency-Key
    P->>P: tx — умовний резерв на платежі, refund pending, outbox refund.created
    P-->>M: 202 Accepted, status pending
    P->>L: POST /internal/v1/holds, reference re_1, amount 6000
    alt коштів достатньо
        L-->>P: 201 hold_5, available → reserved
        P->>B: POST /bank/v1/refunds, Idempotency-Key re_1:refund
        alt банк підтвердив
            B-->>P: 201 approved
            P->>P: tx — refund succeeded, amount_refunded += 6000, outbox refund.succeeded
            K-->>L: refund.succeeded → capture hold, reserved → acquirer_receivable
        else банк відмовив
            B-->>P: 201 declined
            P->>P: tx — refund failed, резерв знято, outbox refund.failed
            K-->>L: refund.failed → release hold, reserved → available
        else таймаут
            P->>P: bank_operation unknown → inquiry, як у флоу 2
        end
    else коштів недостатньо
        L-->>P: 422 insufficient_funds
        P->>P: tx — refund failed insufficient_merchant_balance, резерв знято, outbox refund.failed
    end
    Note over K,M: webhook-service доставить refund.succeeded або refund.failed
```

- **Умовний резерв на платежі** працює без явних локів. Два паралельні повернення по 60 грн на платіж 100 грн: друге отримає 0 оновлених рядків і `422 refund_exceeds_captured`. Як остання лінія оборони стоїть `CHECK` у таблиці.
- **Hold ставимо синхронно**, бо відповідь потрібна до звернення в банк. **Завершуємо через події:** outbox гарантує, що ledger їх отримає, а ledger обробляє їх ідемпотентно.
- **Hold з `reference = re_1` ідемпотентний.** Якщо виклик ledger упав по таймауту, просто повторюємо його — ledger поверне той самий hold.
- **Холди не протухають автоматично.** Якщо сага застрягла в `unknown`, автоматичне звільнення могло б «повернути» мерчанту гроші, які банк уже віддав покупцю. Замість цього є алерт на hold, старший за 24 год, і розбір через reconciliation.

```mermaid
stateDiagram-v2
    [*] --> pending: POST /v1/refunds
    pending --> succeeded: банк підтвердив
    pending --> failed: банк відмовив або бракує коштів
    succeeded --> [*]
    failed --> [*]
```

## Класифікація кроків саг

Термінологія з книги Chris Richardson «Microservices Patterns»:
- **compensatable** — крок, який можна скасувати;
- **pivot** — точка неповернення;
- **retriable** — кроки після pivot, які мусять колись завершитися успіхом. Тому вони ідемпотентні, а ретраяться без ліміту.

| Сага | Крок | Тип | Компенсація |
|---|---|---|---|
| Платіж | Fraud check | compensatable | не потрібна, нічого не змінює |
| Платіж | Authorize у банку | compensatable | void |
| Платіж | Capture у банку | **pivot** | — |
| Платіж | Проводка в ledger, вебхук | retriable | — |
| Повернення | Резерв суми на платежі | compensatable | зняти резерв |
| Повернення | Hold у ledger | compensatable | release через `refund.failed` |
| Повернення | Refund у банку | **pivot** | — |
| Повернення | Capture hold, вебхук | retriable | — |

## Флоу 6. Доставка вебхуків

```mermaid
stateDiagram-v2
    [*] --> pending: подія, на тип якої підписаний endpoint
    pending --> in_flight: воркер забрав пачку, lease 30 с
    in_flight --> succeeded: 2xx за 10 с
    in_flight --> pending: помилка чи таймаут, спроби ще є
    in_flight --> pending: lease минув, воркер упав
    in_flight --> failed: спроби вичерпано
    failed --> pending: ручний retry через API
    succeeded --> [*]
```

- **Розклад:** перша спроба одразу, далі через 1 хв, 5 хв, 30 хв, 2 год, 6 год, 12 год, 24 год і 24 год. Разом ≈ 3 доби, до кожної затримки додається jitter ±20 %.
- **Успіх — тільки 2xx за ≤ 10 с.** Редиректи не виконуємо. `410 Gone` одразу вимикає endpoint, як і 3 доби суцільних невдач.
- **Підпис:** `Kopiyka-Signature: t=<unix-час>,v1=<hex HMAC-SHA256(secret, "<t>.<сире тіло>")>`. Під час ротації секрету передаємо два значення `v1`.
- **Порядок доставки не гарантуємо.** Мерчант дедуплікує події за `Kopiyka-Event-Id`, а актуальний стан завжди може взяти через `GET`.
- **Транзакцію не тримаємо під час HTTP-виклику.** Коротка транзакція забирає пачку (статус `in_flight` + lease), далі йдуть виклики, потім друга коротка транзакція записує результат. Запит на «забрати пачку» — у [05-data-model.md](05-data-model.md).

## Флоу 7. Settlement і reconciliation

```mermaid
sequenceDiagram
    autonumber
    participant J as SettlementJob
    participant B as bank-simulator
    participant S as settlement_db
    participant K as Kafka
    participant L as ledger-service

    Note over J: щодня о 02:00 UTC (у dev — вручну), ShedLock
    J->>B: GET /bank/v1/settlement-reports/2026-10-20
    B-->>J: CSV з операціями за день
    J->>S: крок 1 — рядки звіту в staging
    J->>S: крок 2 — зіставити з expected_operations, проєкцією з payments.events.v1
    J->>S: крок 3 — зафіксувати розбіжності
    J->>S: крок 4 — суми по мерчантах, outbox settlement.completed і discrepancy_found
    K-->>L: settlement.completed → pending → available, receivable → cash
```

Spring Batch зберігає стан job'а в JobRepository. Якщо job упав на кроці 3, перезапуск продовжить з кроку 3, тому кожен крок ідемпотентний.

| Розбіжність | Що означає | Дія |
|---|---|---|
| `missing_in_platform` | банк списав, а в нас `failed` чи `unknown` (ghost) | автоматичний refund покупцю + алерт |
| `missing_in_bank` | у нас `succeeded`, у банку запису немає | inquiry; якщо підтвердилось — ручний розбір |
| `amount_mismatch` | суми не збігаються | ручний розбір + алерт |

## Фонові задачі

| Задача | Сервіс | Частота | Як не задвоїти |
|---|---|---|---|
| Outbox poller (фаза 1) | усі продюсери | 200 мс | один активний інстанс через ShedLock: паралельні паблішери зламають порядок |
| Recovery операцій у `pending` | payment | 10 с | `SKIP LOCKED` |
| Inquiry операцій у `unknown` | payment | 5 с | `SKIP LOCKED` |
| Auto-void прострочених авторизацій | payment | 1 хв | `SKIP LOCKED` |
| Очистка idempotency keys старших за 24 год | payment, webhook | 1 год | ShedLock |
| Доставка вебхуків | webhook | 1 с | `SKIP LOCKED` + lease |
| Trial balance | ledger | 5 хв | ShedLock |
| Settlement і reconciliation | settlement | раз на добу | ShedLock + JobRepository |

## Сценарії збоїв

| Що сталося | Що робить система | Що бачить мерчант |
|---|---|---|
| Мерчант не отримав відповідь і повторив запит | `Idempotency-Key` віддає збережену відповідь | той самий результат + `Idempotent-Replayed: true` |
| Два паралельні confirm одного платежу | умовний `UPDATE`, виграє один | другий отримує `409 payment_unexpected_state` |
| payment-service упав між викликом банку і записом результату | операція лишилась у `pending` → recovery → inquiry → сага продовжується | `processing`, потім вебхук |
| Банк не відповів за 3 с | операція `unknown` → inquiry з backoff | `202 processing`, потім вебхук |
| Банк лежить (circuit breaker відкритий) | у банк не йдемо, платіж повертається в `requires_confirmation` | `503 bank_unavailable` + `Retry-After`. Повтор з тим самим ключем безпечний |
| fraud-service лежить | fallback: до порогу fail-open, вище — fail-closed | платіж з `risk_level: elevated` або `503` |
| Kafka лежить | outbox накопичується, API працює | баланс і вебхуки запізнюються |
| ledger-service лежить | події чекають у Kafka (consumer lag) | баланс оновиться пізніше, гроші не губляться |
| Подія прийшла двічі (рестарт Debezium, ребаланс) | `inbox` або унікальний `external_ref` → no-op | нічого |
| Повідомлення не десеріалізується (poison pill) | `ErrorHandlingDeserializer` → DLT + алерт | нічого |
| Endpoint мерчанта лежить | ретраї ≈ 3 доби, потім `failed` і ручний retry | події доступні через `GET /v1/events` |
| Два часткові повернення разом більші за платіж | умовний резерв, друге не проходить | `422 refund_exceeds_captured` |
| Capture мерчанта збігся з auto-void | умовний перехід, виграє один | `200` або `409` |
| Redis лежить | rate limiter пропускає запити (fail-open), ключі перевіряються напряму в merchant-service | трохи вища латентність |
| Ghost-операцію пропустив inquiry | reconciliation знаходить її у звіті банку → refund покупцю + алерт | нічого: покупець отримує гроші назад |
