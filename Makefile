.DEFAULT_GOAL := help
.PHONY: help up down isolation slowquery mongo cache locking dlock longtx replicas replicas-test partition twosys livemigrate engines acid acid-test pgdyn pgdyn-test dr ids test test-all lint

# The node container writes node_modules into the checkout; run it as you.
export HOST_UID := $(shell id -u)
export HOST_GID := $(shell id -g)

GO := docker compose run --rm go
REPLICA_ENV := -e PRIMARY_URL=postgres://lab:lab@pgprimary:5432/lab?sslmode=disable -e REPLICA_URL=postgres://lab:lab@pgreplica:5432/lab?sslmode=disable

help:          ## list the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/:[^#]*## /\t/' | expand -t 16
up:            ## start the databases most demos use
	docker compose up -d --wait postgres mongo redis
down:          ## stop and remove every container and volume
	docker compose --profile replicas --profile acid --profile dr down -v
isolation: up  ## 01: the isolation-level matrix, with details
	$(GO) go run ./cmd/isolation -details
slowquery: up  ## 02: why queries slow down as the table grows (10K, 1M, 10M rows; several minutes)
	$(GO) go run ./cmd/slowquery -sizes 10000,1000000,10000000
mongo: up      ## 03: Mongoose vs $$jsonSchema vs one collection for everything
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node demo.mjs'
cache: up      ## 04: cache invalidation, races and stampedes (Redis + Postgres)
	$(GO) go run ./cmd/cachedemo
locking: up    ## 05: lock the row vs retry on conflict, at high and low contention
	$(GO) go run ./cmd/lockdemo
dlock: up      ## 06: why a Redis lock is not enough (leases, fencing, advisory locks)
	$(GO) go run ./cmd/dlockdemo
longtx: up     ## 07: one long transaction vs VACUUM (PostgreSQL) and purge (MySQL)
	docker compose up -d --wait mysql
	$(GO) go run ./cmd/longtx
replicas:      ## 08: a streaming replica: lag, stale reads, synchronous_commit, a missing sync replica
	docker compose --profile replicas up -d --wait pgprimary pgreplica
	docker compose run --rm $(REPLICA_ENV) go go run ./cmd/replicas
partition: up  ## 09: one big table vs monthly partitions (6M rows)
	$(GO) go run ./cmd/partition
twosys: up     ## 10: orders and payments in two databases: dual write vs 2PC vs saga vs outbox
	$(GO) go run ./cmd/twosys
livemigrate: up ## 11: schema changes on a 5M-row table under load: blocking vs online
	$(GO) go run ./cmd/livemigrate
engines:       ## 12: the same workload on a B+tree (bbolt) and an LSM tree (Pebble)
	$(GO) go run ./cmd/engines
acid:          ## 13: transfers in PostgreSQL, MongoDB and DynamoDB Local, with and without transactions
	docker compose --profile acid up -d --wait postgres mongors dynamodb
	$(GO) go run ./cmd/acid
pgdyn:         ## 14: the same orders in PostgreSQL and a DynamoDB single-table design
	docker compose --profile acid up -d --wait postgres dynamodb
	$(GO) go run ./cmd/pgdyn
dr:            ## 15: backups and a disaster drill (logical dump vs base backup + WAL archive)
	./15-backup-restore/run.sh
ids:           ## 16: primary keys, bigint vs UUIDv4 vs UUIDv7 (insert time, WAL, index size)
	./16-id-generation/run.sh
test: up       ## the tests CI runs: every demo except 08, 13 and 14 (they need extra containers)
	docker compose up -d --wait mysql
	$(GO) go test -race -count=1 -p 1 ./...
	$(GO) go test -count=1 ./internal/engines/
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node --test'
acid-test:     ## the ACID test (starts the acid profile)
	docker compose --profile acid up -d --wait postgres mongors dynamodb
	$(GO) go test -count=1 ./internal/acid/
pgdyn-test:    ## the PostgreSQL-vs-DynamoDB test (starts DynamoDB Local)
	docker compose --profile acid up -d --wait postgres dynamodb
	$(GO) go test -count=1 ./internal/pgdyn/
replicas-test: ## the replica test (starts the replicas profile)
	docker compose --profile replicas up -d --wait pgprimary pgreplica
	docker compose run --rm $(REPLICA_ENV) go go test -count=1 ./internal/replicas/
test-all: test acid-test pgdyn-test replicas-test ## every test, including the ones that need extra containers
lint:          ## gofmt, go vet, staticcheck, govulncheck, shellcheck, compose config
	docker compose config -q
	$(GO) sh -c 'test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }'
	$(GO) go vet ./...
	$(GO) go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
	$(GO) go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
	docker run --rm -v "$(CURDIR)":/mnt:ro koalaman/shellcheck:v0.10.0 15-backup-restore/run.sh 16-id-generation/run.sh 08-replicas/replica-entrypoint.sh 08-replicas/initdb/10-replication.sh
