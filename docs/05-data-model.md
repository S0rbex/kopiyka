# 05. Модель даних і проводки

Кожен сервіс має власну БД. Міграції лежать у Flyway: `services/<сервіс>/src/main/resources/db/migration/V1__init.sql`. Нижче — цільові схеми ключових таблиць і запити, які треба вміти пояснити рядок за рядком.

## Конвенції

- **Гроші — `bigint` у мінорних одиницях.** Валюта — `char(3)`, час — тільки `timestamptz`.
- **`id` — UUIDv7, генерується в застосунку** (напр. бібліотекою uuid-creator), щоб знати ID ще до вставки і покласти його в подію. Публічний ID — префікс + base62 (`pay_7d1L…`).
- **Перелічення — `text` + `CHECK`, а не `CREATE TYPE … AS ENUM`.** Значення enum'а не можна видалити, тож змінювати такий список болючіше.
- **`version bigint`** у змінюваних агрегатах — для умовних апдейтів і `ETag`.
- **Застосовані міграції не редагуємо ніколи.** Зміни без простою робимо через expand → migrate → contract (тема для фази 6).

## payment_db

```sql
CREATE TABLE payments (
    id                      uuid        PRIMARY KEY,
    merchant_id             uuid        NOT NULL,
    amount                  bigint      NOT NULL CHECK (amount > 0),
    currency                char(3)     NOT NULL,
    capture_method          text        NOT NULL CHECK (capture_method IN ('automatic', 'manual')),
    status                  text        NOT NULL CHECK (status IN ('requires_confirmation', 'processing',
                                            'requires_capture', 'succeeded', 'failed', 'canceled')),
    step                    text,                       -- внутрішній крок саги
    amount_capturable       bigint      NOT NULL DEFAULT 0,
    amount_captured         bigint      NOT NULL DEFAULT 0,
    amount_refunded         bigint      NOT NULL DEFAULT 0,  -- успішні повернення
    amount_refund_reserved  bigint      NOT NULL DEFAULT 0,  -- повернення в процесі
    fee                     bigint,                     -- фіксується в момент capture
    fee_bps                 int,                        -- знімок тарифу
    failure_code            text,
    failure_message         text,
    cancellation_reason     text,
    risk_level              text,
    risk_score              smallint,
    card_brand              text,
    card_last4              char(4),
    card_fingerprint        text,                       -- HMAC-SHA256(PAN); сам PAN не зберігаємо
    bank_authorization_id   text,
    bank_capture_id         text,
    description             text,
    metadata                jsonb       NOT NULL DEFAULT '{}',
    capture_before          timestamptz,
    version                 bigint      NOT NULL DEFAULT 0,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (amount_captured <= amount),
    CHECK (amount_refunded + amount_refund_reserved <= amount_captured)  -- остання лінія оборони від over-refund
);
CREATE INDEX payments_merchant_idx       ON payments (merchant_id, id DESC);   -- списки, keyset-пагінація
CREATE INDEX payments_capture_expiry_idx ON payments (capture_before) WHERE status = 'requires_capture';
CREATE INDEX payments_processing_idx     ON payments (updated_at)     WHERE status = 'processing';  -- алерт на «завислі»

CREATE TABLE refunds (
    id              uuid        PRIMARY KEY,
    payment_id      uuid        NOT NULL REFERENCES payments (id),
    merchant_id     uuid        NOT NULL,
    amount          bigint      NOT NULL CHECK (amount > 0),
    currency        char(3)     NOT NULL,
    status          text        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    step            text,
    reason          text,
    failure_code    text,
    ledger_hold_id  uuid,
    bank_refund_id  text,
    metadata        jsonb       NOT NULL DEFAULT '{}',
    version         bigint      NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refunds_payment_idx  ON refunds (payment_id);
CREATE INDEX refunds_merchant_idx ON refunds (merchant_id, id DESC);

-- Write-ahead intent: рядок комітиться ДО виклику банку
CREATE TABLE bank_operations (
    id                uuid        PRIMARY KEY,
    payment_id        uuid        NOT NULL REFERENCES payments (id),
    refund_id         uuid        REFERENCES refunds (id),
    type              text        NOT NULL CHECK (type IN ('authorize', 'capture', 'void', 'refund')),
    idempotency_key   text        NOT NULL UNIQUE,     -- 'pay_…:authorize', 're_…:refund'
    amount            bigint      NOT NULL,
    status            text        NOT NULL CHECK (status IN ('pending', 'approved', 'declined', 'failed', 'unknown')),
    bank_reference    text,                            -- auth_…, cap_…, rfd_…
    decline_code      text,
    inquiry_attempts  int         NOT NULL DEFAULT 0,
    next_inquiry_at   timestamptz,                     -- для pending ставимо now() + 30 s одразу при вставці
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bank_operations_inquiry_idx ON bank_operations (next_inquiry_at) WHERE status IN ('pending', 'unknown');

CREATE TABLE idempotency_keys (
    merchant_id       uuid        NOT NULL,
    idempotency_key   text        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_method    text        NOT NULL,
    request_path      text        NOT NULL,
    request_hash      bytea       NOT NULL,            -- SHA-256 канонічного тіла
    status            text        NOT NULL CHECK (status IN ('in_progress', 'completed')),
    locked_until      timestamptz,                     -- lease для in_progress
    response_status   int,
    response_headers  jsonb,
    response_body     jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, idempotency_key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);  -- очистка старших за 24 год

-- Фаза 5: тарифи мерчантів з compacted-топіка merchants.state.v1
CREATE TABLE merchant_snapshots (
    merchant_id  uuid        PRIMARY KEY,
    status       text        NOT NULL,
    fee_bps      int         NOT NULL,
    fee_fixed    bigint      NOT NULL,
    version      bigint      NOT NULL,                 -- застосовуємо лише новішу версію
    updated_at   timestamptz NOT NULL
);
```

### Спільні таблиці з `platform-starter`

Є в кожному сервісі, який публікує або споживає події:

```sql
CREATE TABLE outbox (
    id            uuid        PRIMARY KEY,             -- = ce_id події
    destination   text        NOT NULL,                -- 'payments.events' → топік payments.events.v1
    aggregate_id  text        NOT NULL,                -- ключ Kafka-повідомлення
    event_type    text        NOT NULL,                -- 'payment.succeeded'
    payload       jsonb       NOT NULL,                -- у фазі 3 — bytea з Avro
    trace_parent  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz                          -- лише для polling-паблішера (фаза 1)
);
CREATE INDEX outbox_unpublished_idx ON outbox (created_at, id) WHERE published_at IS NULL;

CREATE TABLE inbox (
    consumer      text        NOT NULL,                -- назва consumer group
    event_id      uuid        NOT NULL,                -- ce_id
    processed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE shedlock (
    name        varchar(64)  PRIMARY KEY,
    lock_until  timestamptz  NOT NULL,
    locked_at   timestamptz  NOT NULL,
    locked_by   varchar(255) NOT NULL
);
```

## ledger_db

```sql
CREATE TABLE accounts (
    id              uuid        PRIMARY KEY,
    code            text        NOT NULL UNIQUE,      -- 'merchant:{id}:UAH:available', 'platform:UAH:fees:3'
    owner_type      text        NOT NULL CHECK (owner_type IN ('merchant', 'platform')),
    owner_id        uuid,
    currency        char(3)     NOT NULL,
    type            text        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue')),
    allow_negative  boolean     NOT NULL DEFAULT false,
    balance         bigint      NOT NULL DEFAULT 0,   -- кешований, у напрямку normal balance
    version         bigint      NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (allow_negative OR balance >= 0)            -- мерчант не піде в мінус навіть через баг у коді
);

CREATE TABLE ledger_transactions (
    id            uuid        PRIMARY KEY,
    external_ref  text        NOT NULL UNIQUE,        -- 'payment:pay_…:capture' — ідемпотентність проводки
    type          text        NOT NULL,               -- capture | refund_hold | refund_capture | refund_release
                                                      -- | settlement | payout | adjustment
    merchant_id   uuid,
    currency      char(3)     NOT NULL,
    occurred_at   timestamptz NOT NULL,               -- бізнес-час з події
    created_at    timestamptz NOT NULL DEFAULT now(),
    metadata      jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX ledger_transactions_merchant_idx ON ledger_transactions (merchant_id, id DESC);  -- /v1/balance_transactions

CREATE TABLE ledger_entries (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id  uuid        NOT NULL REFERENCES ledger_transactions (id),
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    direction       text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount          bigint      NOT NULL CHECK (amount > 0),
    balance_after   bigint      NOT NULL,             -- баланс рахунку після проводки: виписки й аудит
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, id DESC);
CREATE INDEX ledger_entries_tx_idx      ON ledger_entries (transaction_id);

-- Незмінність на рівні прав: у ролі застосунку немає UPDATE і DELETE
REVOKE UPDATE, DELETE ON ledger_entries, ledger_transactions FROM ledger_app;

CREATE TABLE holds (
    id          uuid        PRIMARY KEY,
    reference   text        NOT NULL UNIQUE,          -- 're_…' — ідемпотентність створення
    account_id  uuid        NOT NULL REFERENCES accounts (id),   -- available-рахунок мерчанта
    amount      bigint      NOT NULL CHECK (amount > 0),
    status      text        NOT NULL CHECK (status IN ('active', 'captured', 'released')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX holds_active_idx ON holds (created_at) WHERE status = 'active';   -- алерт на hold старший за 24 год
```

### Рахунки

| Рахунок | Тип | Що означає |
|---|---|---|
| `merchant:{id}:{cur}:pending` | liability | зароблено, settlement ще не підтвердив |
| `merchant:{id}:{cur}:available` | liability | можна повертати й виплачувати |
| `merchant:{id}:{cur}:reserved` | liability | зарезервовано під повернення |
| `platform:{cur}:acquirer_receivable` | asset, `allow_negative` | скільки нам винен банк |
| `platform:{cur}:cash` | asset, `allow_negative` | гроші на рахунку платформи |
| `platform:{cur}:fees:{0..7}` | revenue | комісія. Шардована на 8 субрахунків, бо це hot account (фаза 6) |

**Normal balance:**
- `asset`: `balance += debit − credit`;
- `liability` і `revenue`: `balance += credit − debit`.

Звідси інваріант trial balance: Σ asset = Σ liability + Σ revenue по кожній валюті.

Рахунки мерчанта створюються ліниво при першій проводці: `INSERT … ON CONFLICT (code) DO NOTHING`.

### Posting rules

Приклад: платіж 100,50 грн, комісія 2,51 грн, повернення 60,00 грн. Суми в копійках.

| Подія | Дебет | Кредит |
|---|---|---|
| `payment.succeeded` (10050, fee 251) | `acquirer_receivable` 10050 | `merchant:pending` 9799; `platform:fees` 251 |
| Hold під повернення (6000), синхронний API | `merchant:available` 6000 | `merchant:reserved` 6000 |
| `refund.succeeded` | `merchant:reserved` 6000 | `acquirer_receivable` 6000 |
| `refund.failed`, якщо hold був | `merchant:reserved` 6000 | `merchant:available` 6000 |
| `settlement.completed` | `platform:cash` bank_net; `merchant:pending` captured_net | `acquirer_receivable` bank_net; `merchant:available` captured_net |
| Виплата *(опційно)* | `merchant:available` X | `platform:cash` X |

- До фази 5 settlement ще немає, тому `payment.succeeded` кредитує одразу `merchant:available`.
- Комісія за повернення мерчанту не повертається: це бізнес-правило, його треба зафіксувати в ADR.
- Перевірка першого рядка: дебет 10050 = кредит 9799 + 251, а trial balance дає 10050 (asset) = 9799 (liability) + 251 (revenue).

## merchant_db

```sql
CREATE TABLE merchants (
    id                     uuid        PRIMARY KEY,
    name                   text        NOT NULL,
    status                 text        NOT NULL CHECK (status IN ('active', 'suspended')),
    fee_bps                int         NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    fee_fixed              bigint      NOT NULL DEFAULT 0,
    settlement_delay_days  int         NOT NULL DEFAULT 1,
    version                bigint      NOT NULL DEFAULT 0,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id            uuid        PRIMARY KEY,
    merchant_id   uuid        NOT NULL REFERENCES merchants (id),
    key_hash      bytea       NOT NULL UNIQUE,        -- SHA-256 від повного ключа
    prefix        text        NOT NULL,               -- 'sk_test_'
    last4         char(4)     NOT NULL,
    status        text        NOT NULL CHECK (status IN ('active', 'revoked')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,                        -- оновлюємо не частіше разу на хвилину, щоб не писати на кожен запит
    revoked_at    timestamptz
);
-- + outbox (merchants.state, merchants.events)
```

## webhook_db

```sql
CREATE TABLE webhook_endpoints (
    id                          uuid        PRIMARY KEY,
    merchant_id                 uuid        NOT NULL,
    url                         text        NOT NULL,
    enabled_events              text[]      NOT NULL,
    description                 text,
    status                      text        NOT NULL CHECK (status IN ('enabled', 'disabled')),
    disabled_reason             text,
    secret_encrypted            bytea       NOT NULL,   -- AES-GCM: секрет потрібен для підпису, тому не хеш
    previous_secret_encrypted   bytea,
    previous_secret_expires_at  timestamptz,
    consecutive_failures        int         NOT NULL DEFAULT 0,
    version                     bigint      NOT NULL DEFAULT 0,   -- → ETag
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id);

CREATE TABLE events (                                   -- публічні події: GET /v1/events, зберігаються 30 днів
    id               uuid        PRIMARY KEY,
    merchant_id      uuid        NOT NULL,
    type             text        NOT NULL,
    data             jsonb       NOT NULL,              -- знімок публічного об'єкта
    source_event_id  uuid        NOT NULL UNIQUE,       -- ce_id внутрішньої події → ідемпотентність
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_merchant_idx ON events (merchant_id, id DESC);

CREATE TABLE webhook_deliveries (
    id                uuid        PRIMARY KEY,
    event_id          uuid        NOT NULL REFERENCES events (id),
    endpoint_id       uuid        NOT NULL REFERENCES webhook_endpoints (id),
    status            text        NOT NULL CHECK (status IN ('pending', 'in_flight', 'succeeded', 'failed')),
    attempt_count     int         NOT NULL DEFAULT 0,  -- заодно fencing token
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),  -- для in_flight — момент, коли lease закінчиться
    last_http_status  int,
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status IN ('pending', 'in_flight');

CREATE TABLE webhook_delivery_attempts (                -- журнал спроб для діагностики
    id                bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_id       uuid        NOT NULL REFERENCES webhook_deliveries (id),
    attempt_no        int         NOT NULL,
    http_status       int,
    duration_ms       int,
    error             text,
    response_snippet  text,                             -- перший 1 КБ відповіді
    created_at        timestamptz NOT NULL DEFAULT now()
);
```

## settlement_db (фаза 5)

| Таблиця | Що зберігає |
|---|---|
| `expected_operations` | проєкція з `payments.events.v1`: `bank_reference` (PK), `type`, `payment_id`, `refund_id`, `merchant_id`, `amount`, `currency`, `net_to_merchant`, `occurred_at`, `settlement_id` |
| `bank_report_lines` | staging-рядки зі звіту банку + `matched` |
| `settlements` | підсумки по мерчанту й валюті за день, `UNIQUE (merchant_id, currency, settlement_date)` |
| `discrepancies` | `kind`, `bank_reference`, `payment_id`, `expected_amount`, `actual_amount`, `status`, `resolution` |
| службові | таблиці Spring Batch (`BATCH_JOB_INSTANCE`, …), `outbox`, `inbox`, `shedlock` |

## Ключові запити

### 1. Умовний перехід стану (без явних локів)

```sql
UPDATE payments
SET    status = 'processing', step = 'fraud_check', version = version + 1, updated_at = now()
WHERE  id = :id AND merchant_id = :merchantId AND status = 'requires_confirmation'
RETURNING version;
-- 0 рядків → 409 payment_unexpected_state
```

### 2. Резерв під повернення

```sql
UPDATE payments
SET    amount_refund_reserved = amount_refund_reserved + :amount, version = version + 1, updated_at = now()
WHERE  id = :paymentId AND merchant_id = :merchantId AND status = 'succeeded'
  AND  amount_captured - amount_refunded - amount_refund_reserved >= :amount;
-- 0 рядків → 422 refund_exceeds_captured
```

**Чому два паралельні повернення не пройдуть обидва.** Це питання з співбесід. `UPDATE` бере блокування рядка, тож друга транзакція чекає першу. У `READ COMMITTED` Postgres після очікування перечитує **нову** версію рядка і заново перевіряє `WHERE`. Умова вже не виконується, і оновлено 0 рядків.

### 3. Проводка в ledger без дублів і deadlock'ів

Усе нижче — одна транзакція.

```sql
-- 1) Ідемпотентність: проводка вже є → нічого не робимо
INSERT INTO ledger_transactions (id, external_ref, type, merchant_id, currency, occurred_at)
VALUES (:id, :externalRef, :type, :merchantId, :currency, :occurredAt)
ON CONFLICT (external_ref) DO NOTHING
RETURNING id;                                  -- 0 рядків → дублікат події, виходимо

-- 2) Блокуємо всі рахунки транзакції в одному порядку (за id): зустрічні проводки не дадуть deadlock
SELECT id, type, balance FROM accounts WHERE id = ANY(:accountIds) ORDER BY id FOR UPDATE;

-- 3) Оновлюємо баланси й пишемо проводки з balance_after
--    (у коді перед цим перевіряємо Σ debit = Σ credit)
UPDATE accounts SET balance = balance + :delta, version = version + 1 WHERE id = :accountId;
INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, balance_after)
VALUES (:txId, :accountId, :direction, :amount, :balanceAfter);
```

### 4. Keyset-пагінація

```sql
SELECT *
FROM   payments
WHERE  merchant_id = :merchantId
  AND  id < :cursorId                          -- умова є лише на другій і наступних сторінках
  AND  status = :status                        -- лише якщо передано фільтр
ORDER  BY id DESC                              -- UUIDv7 впорядкований за часом
LIMIT  :limit + 1;                             -- +1, щоб знати has_more
```

- Запит будуємо динамічно з тих умов, які реально передано. Варіант `(:x IS NULL OR …)` псує план запиту.
- Якщо фільтр за статусом популярний, додаємо індекс `(merchant_id, status, id DESC)`.

### 5. Забрати пачку вебхуків (claim + lease)

```sql
UPDATE webhook_deliveries d
SET    status          = 'in_flight',
       attempt_count   = d.attempt_count + 1,
       next_attempt_at = now() + interval '30 seconds',   -- lease: якщо воркер упаде, рядок знову стане доступним
       updated_at      = now()
WHERE  d.id IN (SELECT id
                FROM   webhook_deliveries
                WHERE  status IN ('pending', 'in_flight') AND next_attempt_at <= now()
                ORDER  BY next_attempt_at
                LIMIT  50
                FOR UPDATE SKIP LOCKED)
RETURNING d.*;
```

- Транзакція тут коротка і комітиться **до** HTTP-викликів.
- Результат записується умовно: `… WHERE id = :id AND attempt_count = :claimedAttempt`. `attempt_count` працює як **fencing token**: воркер, що «прокинувся» після закінчення lease, не перезапише результат новішої спроби.
- Той самий патерн claim + lease використовують `InquiryJob` (за `next_inquiry_at`) і `CaptureExpiryJob`.

### 6. Trial balance

```sql
SELECT currency,
       sum(CASE WHEN type = 'asset' THEN balance ELSE -balance END) AS diff   -- має дорівнювати 0
FROM   accounts
GROUP  BY currency;
```

Метрика `ledger_trial_balance_diff` — це результат цього запиту. Будь-яке ненульове значення одразу дає алерт.

### 7. Міжсервісний інваріант (e2e-тести, фаза 2)

Тестовий harness має доступ до обох БД і перевіряє:
- кожен `succeeded` платіж у `payment_db` має **рівно одну** транзакцію `payment:{id}:capture` у `ledger_db`;
- жоден платіж не висить у `processing` довше 5 хвилин.

У продакшн-коді таких крос-БД запитів немає, у проді цю роль виконує reconciliation.
