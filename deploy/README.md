# deploy

Локальний запуск через Docker Compose.

## Що з'явиться у фазі 0

- `docker-compose.yml`, профіль `infra`:
  - Postgres 18
  - Kafka 4 у KRaft з `auto.create.topics.enable=false`
  - Redis
  - Kafka UI
  - `grafana/otel-lgtm`
- `postgres/init.sql`: окрема БД і користувач для кожного сервісу (`payment`, `ledger`, `merchant`)

## Команди (з папки `deploy/`)

| Що | Команда |
|---|---|
| підняти інфраструктуру у фоні | `docker compose --profile infra up -d` |
| статус і healthcheck-и | `docker compose ps` |
| логи одного сервісу | `docker compose logs -f kafka` |
| зупинити (дані Postgres лишаються) | `docker compose --profile infra down` |
| зупинити й **стерти** дані | `docker compose --profile infra down -v` |

`init.sql` виконується тільки при першому старті, коли том з даними порожній. Змінив `init.sql` — роби `down -v`.

| UI | Адреса |
|---|---|
| Kafka UI | http://localhost:8090 |
| Grafana | http://localhost:3000 |

Порти 5432, 9092, 6379, 8090 і 3000 мають бути вільні. Якщо там працює інший Docker-стек, спершу зупини його.
