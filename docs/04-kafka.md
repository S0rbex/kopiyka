# 04. Kafka: топіки, події, гарантії

## 1. Навіщо тут Kafka

- **Один факт — кілька незалежних споживачів.** Про «платіж списано» мають дізнатися ledger, webhook, fraud і settlement. Кожен читає через свою consumer group і у своєму темпі. Падіння одного не зупиняє інших.
- **Порядок у межах платежу:** ключ повідомлення — `payment_id`.
- **Replay.** Новий консюмер (settlement-service) будує проєкцію з історії. Після бага подію можна перечитати.
- **Debezium.** CDC з Postgres прямо в Kafka.

**Через Kafka не йде:**
- запити, на які відповідь потрібна зараз (fraud, банк, hold) — це REST;
- черга ретраїв вебхуків — таблиця в БД (див. [01-architecture.md](01-architecture.md), рішення 8).

## 2. Кластер

| Параметр | dev | Фаза 6 (prod-like) |
|---|---|---|
| Брокери | 1, KRaft (combined mode) | 3, KRaft |
| `replication.factor` | 1 | 3 |
| `min.insync.replicas` | 1 | 2 |
| Продюсери | `acks=all`, `enable.idempotence=true` | те саме |
| `auto.create.topics.enable` | `false` | `false` |

- **Топіки створюються явно:** контейнер `kafka-init` запускає `kafka-topics.sh --create --if-not-exists`, у Kubernetes це Strimzi `KafkaTopic`.
- **Автостворення вимкнене.** Друкарська помилка в назві топіка має давати помилку, а не створювати новий топік з дефолтними налаштуваннями.

## 3. Іменування

Шаблон — `<домен>.<тип>.v<N>`:
- `payments.events.v1` — потік подій, `cleanup.policy=delete`;
- `merchants.state.v1` — снапшоти стану, `cleanup.policy=compact`;
- `<топік>.<consumer-group>.dlt` — DLT конкретного консюмера;
- `.v<N>` — версія контракту. Breaking change означає новий топік і період подвійної публікації.

## 4. Каталог топіків

| Топік | Ключ | Партицій (dev) | Політика | Продюсер | Консюмери (group) | Фаза |
|---|---|---|---|---|---|---|
| `payments.events.v1` | `payment_id` | 3 | delete, 7 днів | payment-service (outbox) | `ledger-service`, `webhook-service`, `fraud-service`, `settlement-service` | 1 |
| `ledger.events.v1` | `merchant_id` | 3 | delete, 7 днів | ledger-service (outbox) | `webhook-service` (подія `balance.available`) | 5 |
| `settlements.events.v1` | `merchant_id` | 3 | delete, 30 днів | settlement-service (outbox) | `ledger-service` | 5 |
| `merchants.state.v1` | `merchant_id` | 3 | **compact** | merchant-service (outbox) | `payment-service` | 5 |
| `merchants.events.v1` | `merchant_id` | 3 | delete, 7 днів | merchant-service (outbox) | `api-gateway` (інвалідація кешу ключів) | 6 |
| `fx.rates.v1` *(опційно)* | `USD:UAH` | 1 | **compact** | fx-service (без outbox) | `fraud-service` | 4 |
| `<топік>.<group>.dlt` | як в оригіналі | 1 | delete, 30 днів | error handler Spring Kafka | людина + алерт | 2 |
| `connect-configs`, `connect-offsets`, `connect-status` | — | — | compact | Kafka Connect | — | 2 |

Нюанси:
- **Події повернень ідуть у `payments.events.v1` з ключем `payment_id`.** Порядок гарантований лише в межах партиції, між топіками його немає. Якби повернення жили в окремому топіку, ledger міг би побачити `refund.succeeded` раніше за `payment.succeeded`.
- **fx-service не потребує outbox.** Він щоразу публікує повний снапшот курсу в compacted-топік, тож втрачене оновлення перекриється наступним.

## 5. Конверт події

Метадані лежать у Kafka-заголовках (мінімальний набір у дусі CloudEvents binary mode), payload — JSON. Фаза 3 переводить payload на Avro.

| Заголовок | Приклад | Навіщо |
|---|---|---|
| `ce_id` | `0192f5c8-7b3e-7a10-9c4d-2e8f1a6b3c5d` (UUIDv7) | ключ дедуплікації для inbox |
| `ce_type` | `payment.succeeded` | диспетчеризація без парсингу payload |
| `traceparent` | `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01` | наскрізний трейс через Kafka |

Кожен payload містить `occurred_at` — бізнес-час події, а не час публікації.

## 6. Каталог подій

### `payments.events.v1`

| Тип | Коли | Ключові поля |
|---|---|---|
| `payment.created` | платіж створено | `amount`, `currency`, `capture_method` |
| `payment.authorized` | authorize пройшов (manual capture) | `amount_capturable`, `capture_before` |
| `payment.succeeded` | capture пройшов (pivot) | `amount_captured`, `fee`, `net`, `bank.capture_id` |
| `payment.failed` | відмова, fraud або помилка | `failure_code` |
| `payment.canceled` | cancel, void або протермінування | `cancellation_reason` |
| `refund.created` | повернення прийнято | `refund_id`, `amount` |
| `refund.succeeded` | банк підтвердив повернення | `refund_id`, `amount`, `bank.refund_id` |
| `refund.failed` | відмова або нестача коштів | `refund_id`, `failure_code`, `hold_id` (може бути `null`) |

```json
{
  "payment_id": "pay_7d1Lr0tRkX2b9QeVwYz4",
  "merchant_id": "mer_2fQe8Kd0aLm3",
  "amount": 10050,
  "amount_captured": 10050,
  "currency": "UAH",
  "fee": 251,
  "net": 9799,
  "capture_method": "automatic",
  "card": { "brand": "visa", "last4": "4242" },
  "bank": { "authorization_id": "auth_01JB7Q3V9X", "capture_id": "cap_01JB7Q8K2M" },
  "metadata": { "order_id": "1001" },
  "payment_version": 3,
  "occurred_at": "2026-10-21T10:15:30.120Z"
}
```

Внутрішні події **не дорівнюють** публічним вебхукам. Внутрішні містять `merchant_id`, банківські ID і комісію. webhook-service перетворює їх на публічні об'єкти з [public-api.yaml](../api/openapi/public-api.yaml), тобто працює як anti-corruption layer. Тому внутрішній контракт можна змінювати, не ламаючи мерчантів.

### Інші топіки

| Топік | Тип | Поля |
|---|---|---|
| `ledger.events.v1` | `ledger.transaction.posted` | `transaction_id`, `external_ref`, `type`, `merchant_id`, `currency`, `balances` (`available`, `pending`, `reserved`) |
| `settlements.events.v1` | `settlement.completed` | `settlement_id`, `merchant_id`, `currency`, `settlement_date`, `captured_net`, `bank_net`, `items_count` |
| `settlements.events.v1` | `reconciliation.discrepancy_found` | `discrepancy_id`, `kind`, `payment_id`, `bank_operation_id`, `expected_amount`, `actual_amount` |
| `merchants.state.v1` | повний снапшот | `merchant_id`, `status`, `fee_bps`, `fee_fixed`, `settlement_delay_days`, `version`; при видаленні — tombstone (`null`) |
| `merchants.events.v1` | `api_key.created`, `api_key.revoked` | `api_key_id`, `merchant_id`, `key_hash` |
| `fx.rates.v1` | снапшот курсу | `base`, `quote`, `rate` (рядок `"41.5000"`, не float), `source`, `as_of` |

## 7. Гарантії

### Порядок

- **Порядок гарантований лише в межах партиції.** Ключ `payment_id` кладе всі події платежу в одну партицію, тож вони йдуть по черзі.
- **Idempotent producer** зберігає порядок навіть при ретраях (`max.in.flight.requests.per.connection ≤ 5`).
- **Debezium читає WAL у порядку комітів.** У polling-паблішера має бути один активний інстанс і сортування `created_at, id`.
- **Кількість партицій плануємо заздалегідь.** Якщо її збільшити, відповідність «ключ → партиція» зміниться, і порядок для старих ключів зламається.

### Доставка

- **At-least-once.** Продюсер ретраїть, консюмер комітить offset після обробки. Отже, дублікати можливі: рестарт Debezium, ребаланс, падіння після обробки, але до коміту.
- **Effectively-once** забезпечують ідемпотентні консюмери:
  - запис в `inbox` у тій самій транзакції БД, що й бізнес-ефект;
  - природні унікальні ключі, як-от `external_ref` у ledger.
- **Exactly-once у Kafka** (transactions + `isolation.level=read_committed`) працює тільки для схеми Kafka → Kafka (consume-transform-produce). Наші побічні ефекти живуть у БД і HTTP, тому там рятує ідемпотентність, а не транзакції Kafka. EOS знадобиться лише в опційному analytics-streams у фазі 6 (`processing.guarantee=exactly_once_v2`).

## 8. Outbox

### Фаза 1: polling publisher

1. Бізнес-зміна і рядок `outbox` записуються в одній транзакції (DDL — у [05-data-model.md](05-data-model.md)).
2. Поллер раз на 200 мс бере `WHERE published_at IS NULL ORDER BY created_at, id LIMIT 500`, відправляє повідомлення, чекає acks і проставляє `published_at`.
3. Активний **один** інстанс (ShedLock). Два паралельні поллери зламають порядок.
4. Якщо поллер упав між відправкою і `UPDATE`, буде дублікат. Це нормально: консюмери ідемпотентні.
5. Беремо рядки за `published_at IS NULL`, а **не** за «`created_at` більший за останній бачений». Довга транзакція може закомітитись пізніше з меншим `created_at`, і її подія загубиться.
6. Опубліковані рядки старші за 7 днів видаляються.

### Фаза 2: Debezium CDC

- **Postgres:** `wal_level=logical`, окремий користувач з `REPLICATION`, публікація лише для таблиці `outbox`.
- **Outbox-рядок можна видаляти в тій самій транзакції одразу після вставки.** INSERT уже потрапив у WAL, а Outbox Event Router ігнорує DELETE. Таблиця не росте.

```json
{
  "name": "payment-outbox",
  "config": {
    "connector.class": "io.debezium.connector.postgresql.PostgresConnector",
    "plugin.name": "pgoutput",
    "database.hostname": "postgres",
    "database.port": "5432",
    "database.user": "debezium",
    "database.password": "${file:/secrets/debezium.properties:password}",
    "database.dbname": "payment_db",
    "topic.prefix": "payment",
    "table.include.list": "public.outbox",
    "publication.autocreate.mode": "filtered",
    "tombstones.on.delete": "false",
    "transforms": "outbox",
    "transforms.outbox.type": "io.debezium.transforms.outbox.EventRouter",
    "transforms.outbox.route.by.field": "destination",
    "transforms.outbox.route.topic.replacement": "${routedByValue}.v1",
    "transforms.outbox.table.field.event.key": "aggregate_id",
    "transforms.outbox.table.field.event.payload": "payload",
    "transforms.outbox.table.expand.json.payload": "true",
    "transforms.outbox.table.fields.additional.placement": "id:header:ce_id,event_type:header:ce_type,trace_parent:header:traceparent",
    "key.converter": "org.apache.kafka.connect.storage.StringConverter",
    "value.converter": "org.apache.kafka.connect.json.JsonConverter",
    "value.converter.schemas.enable": "false"
  }
}
```

- Колонка `destination` = `payments.events` дає топік `payments.events.v1`. Так один outbox обслуговує кілька топіків сервісу: у merchant-service це `merchants.state` і `merchants.events`.
- Підстановка `${file:…}` потребує `config.providers=file` у налаштуваннях Kafka Connect.

## 9. Консюмери

### Групи і стратегія ретраїв

| Консюмер | Чи важливий порядок | Стратегія |
|---|---|---|
| `ledger-service` | так | **blocking retry**: `DefaultErrorHandler`, exponential backoff до 1 хв, без ліміту спроб, + алерт. У DLT — лише poison pill |
| `settlement-service` | так (проєкція) | blocking retry, як у ledger |
| `webhook-service` | ні | `@RetryableTopic`: 1 с → 10 с → 1 хв, далі DLT |
| `fraud-service` (Python) | ні | best-effort: 3 спроби, потім лог і пропуск. Фічі некритичні |
| `api-gateway` | ні | best-effort: якщо подію пропустили, кеш однаково протухне за 60 с |

**Чому для ledger не підходять retry topics.** Retry topic виносить повідомлення з основного потоку. Поки `payment.succeeded` чекає в retry-топіку, наступний `refund.succeeded` того самого платежу вже обробиться. Для грошей це неприпустимо. Краще зупинити партицію і підняти алерт, ніж провести проводки в неправильному порядку.

### Конфігурація Spring Kafka (ledger-service)

```yaml
spring:
  kafka:
    consumer:
      group-id: ledger-service
      auto-offset-reset: earliest
      enable-auto-commit: false
      max-poll-records: 100
      key-deserializer: org.springframework.kafka.support.serializer.ErrorHandlingDeserializer
      value-deserializer: org.springframework.kafka.support.serializer.ErrorHandlingDeserializer
      properties:
        spring.deserializer.key.delegate.class: org.apache.kafka.common.serialization.StringDeserializer
        spring.deserializer.value.delegate.class: org.apache.kafka.common.serialization.StringDeserializer
        group.protocol: consumer   # новий протокол ребалансу Kafka 4 (KIP-848)
    listener:
      ack-mode: record             # коміт після кожного запису: мінімум повторів після падіння
    producer:
      acks: all
      properties:
        enable.idempotence: true
        compression.type: zstd
        linger.ms: 5
```

```java
@Bean
DefaultErrorHandler ledgerErrorHandler(KafkaTemplate<Object, Object> template) {
    var recoverer = new DeadLetterPublishingRecoverer(template,
            (rec, ex) -> new TopicPartition(rec.topic() + ".ledger-service.dlt", -1));
    var backOff = new ExponentialBackOff(1_000, 2.0);
    backOff.setMaxInterval(60_000); // далі раз на хвилину, без ліміту спроб
    return new DefaultErrorHandler(recoverer, backOff);
}
```

- **`DeserializationException` за замовчуванням не ретраїться.** Poison pill одразу йде в DLT, а консюмер працює далі.
- **Довгий backoff не повинен перевищувати `max.poll.interval.ms`,** інакше брокер вважатиме консюмер мертвим і почне ребаланс. У Spring Kafka для цього є `ContainerPausingBackOffHandler`, який ставить консюмер на паузу замість сну. Перевір його у фазі 2.
- **Повідомлення з DLT** переглядаємо в Kafka UI. Після виправлення бага повертаємо їх в основний топік невеликою admin-командою.

### Ретраї у webhook-service

```java
@RetryableTopic(attempts = "4",
        backoff = @Backoff(delay = 1_000, multiplier = 10, maxDelay = 60_000),
        retryTopicSuffix = ".webhook-service.retry",
        dltTopicSuffix = ".webhook-service.dlt")
@KafkaListener(topics = "payments.events.v1", groupId = "webhook-service")
void onPaymentEvent(ConsumerRecord<String, String> record) { /* … */ }
```

**Суфікс з назвою групи обов'язковий.** Без нього retry-топіки спільні для всіх груп, які слухають той самий топік через `@RetryableTopic`. Тоді повідомлення, яке ретраїть одна група, отримає й інша.

### Python-консюмер (fraud-service)

- Бібліотека `confluent-kafka`, `enable.auto.commit=false`.
- Синхронний `commit()` після обробки пачки.
- `ce_type` читаємо із заголовків і обробляємо лише потрібні типи. Решту пропускаємо: так працює tolerant reader.

## 10. Еволюція схем (фаза 3)

- **Schema Registry** — Confluent або Apicurio з Confluent-сумісним API. Формат — Avro.
- **`TopicRecordNameStrategy`.** У топіку кілька типів подій, і кожен має свій subject, напр. `payments.events.v1-com.kopiyka.events.PaymentSucceeded`.
- **Сумісність `BACKWARD`:** новий консюмер читає старі дані. Правила:
  - нові поля — тільки з `default`;
  - поле без `default` не видаляємо;
  - тип поля не змінюємо;
  - перейменування — через `aliases`.
- **Перевірка сумісності в CI** до деплою: несумісна схема не дійде до реєстру.
- **Outbox з Avro.** Застосунок серіалізує подію в Avro (`KafkaAvroSerializer` реєструє схему) і кладе байти в `outbox.payload bytea`. Debezium пересилає їх як є через `ByteArrayConverter`. Тут легко застрягнути, тому відведи на це не більше 2 днів. Запасний варіант — JSON Schema замість Avro.
- **Консюмери:**
  - Java — `KafkaAvroDeserializer` + `specific.avro.reader=true`, обробник на кожен тип через `@KafkaHandler`;
  - Python — `AvroDeserializer` з `confluent_kafka.schema_registry`.
- **Breaking change** означає топік `payments.events.v2`: деякий час публікуємо в обидва, консюмери переходять по одному.

## 11. Compacted-топіки: event-carried state transfer

- `merchants.state.v1` зберігає для кожного ключа останній снапшот. При старті консюмер читає топік з початку і будує локальну таблицю. Далі просто застосовує оновлення.
- Так payment-service отримує тарифи мерчантів без синхронного виклику: merchant-service може лежати, а платежі проходять.
- Видалення робиться tombstone-повідомленням (`value = null`). Tombstone живе `delete.retention.ms` (1 доба), щоб усі консюмери встигли його побачити.
- `min.compaction.lag.ms` гарантує, що свіжі записи не стиснуться раніше, ніж їх прочитають.

## 12. Моніторинг

| Метрика | Навіщо | Алерт |
|---|---|---|
| Consumer lag за групами | ledger, вебхуки й баланси відстають | lag ledger > 1000 протягом 5 хв |
| Повідомлення в `*.dlt` | poison pill або баг | будь-яке |
| `outbox_lag_seconds` | Kafka чи Connect не публікує | > 30 с |
| Статус конектора Debezium | конектор упав, outbox мовчить | не `RUNNING` |
| Under-replicated partitions (фаза 6) | брокер відстає або лежить | > 0 |

Для ручного розбору є Kafka UI (`kafbat/kafka-ui`, порт 8090).

## 13. Питання на співбесіді, на які відповідає цей дизайн

- **«Як гарантуєте порядок?»** Ключ = `payment_id`, повернення в тому самому топіку, idempotent producer, Debezium у порядку комітів, blocking retry в ledger.
- **«Консюмер упав після обробки, але до коміту offset — що буде?»** Повідомлення прийде ще раз. Inbox або `external_ref` зроблять повторну обробку no-op.
- **«У вас exactly-once?»** Наскрізного exactly-once не буває. Є at-least-once доставка + ідемпотентні консюмери. Kafka EOS — тільки для Kafka → Kafka.
- **«Чому не retry topics усюди?»** Вони ламають порядок. Для ledger — blocking retry, для вебхуків — retry topics.
- **«Як змінити формат події, не поклавши консюмерів?»** Schema Registry, `BACKWARD`-сумісність, поля з default. Breaking change — новий топік `.v2`.
- **«Що таке log compaction і навіщо?»** Топік як реплікована таблиця «останній стан за ключем». Приклад — `merchants.state.v1`.
- **«Що буде, якщо Kafka ляже?»** API працює, outbox накопичує події, після відновлення все доїде. Алерт спрацює на `outbox_lag_seconds`.
