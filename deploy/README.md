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

Запуск: `docker compose --profile infra up`
