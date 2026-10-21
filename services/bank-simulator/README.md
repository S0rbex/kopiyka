# bank-simulator (Go)

«Зовнішній банк» з керованим хаосом. Суть: [docs/services/bank-simulator.md](../../docs/services/bank-simulator.md). Контракт: [api/openapi/bank-simulator.yaml](../../api/openapi/bank-simulator.yaml).

Збирається своїм інструментом (`go build`), Maven його не бачить.

| Папка | Що тут живе |
|---|---|
| `cmd/bank-simulator/` | `main.go`: точка входу, запуск HTTP-сервера |
| `internal/api/` | код, згенерований oapi-codegen з OpenAPI, і хендлери |
| `internal/bank/` | логіка банку: authorize, capture, void, inquiry, тестові картки |
| `internal/chaos/` | керовані збої: таймаути, ghost-операції (фаза 2) |

`internal/` у Go означає, що ці пакети не можна імпортувати ззовні модуля.

## Що з'явиться у фазі 0

- `go.mod`
- `main.go` з ендпоінтом `/health` (порт 9090, змінна `PORT`)
