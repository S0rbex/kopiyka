# CI

У фазі 0 тут з'явиться `ci.yml` з двома job-ами:
- **java:** `mvn verify` (збірка, тести, Spotless, ArchUnit)
- **go:** `go build` і `go test` для bank-simulator
