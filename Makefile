.PHONY: up down isolation slowquery mongo cache locking dlock longtx replicas replicas-test partition twosys livemigrate engines acid acid-test pgdyn pgdyn-test dr test
up:            ## start the databases
	docker compose up -d --wait postgres mongo redis
down:          ## stop and remove everything
	docker compose down -v
isolation: up  ## 01: the isolation-level matrix, with details
	docker compose run --rm go go run ./cmd/isolation -details
slowquery: up  ## 02: why queries slow down as the table grows (10K, 1M, 10M rows; several minutes)
	docker compose run --rm go go run ./cmd/slowquery -sizes 10000,1000000,10000000
mongo: up      ## 03: Mongoose vs $$jsonSchema vs one collection for everything
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node demo.mjs'
cache: up      ## 04: cache invalidation, races and stampedes (Redis + Postgres)
	docker compose run --rm go go run ./cmd/cachedemo
locking: up    ## 05: lock the row vs retry on conflict, at high and low contention
	docker compose run --rm go go run ./cmd/lockdemo
dlock: up      ## 06: why a Redis lock is not enough (leases, fencing, advisory locks)
	docker compose run --rm go go run ./cmd/dlockdemo
longtx: up     ## 07: one long transaction vs VACUUM (PostgreSQL) and purge (MySQL)
	docker compose up -d --wait mysql
	docker compose run --rm go go run ./cmd/longtx
replicas:      ## 08: a streaming replica: lag, stale reads, synchronous_commit, a missing sync replica
	docker compose --profile replicas up -d --wait pgprimary pgreplica
	docker compose run --rm -e PRIMARY_URL=postgres://lab:lab@pgprimary:5432/lab?sslmode=disable -e REPLICA_URL=postgres://lab:lab@pgreplica:5432/lab?sslmode=disable go go run ./cmd/replicas
partition: up  ## 09: one big table vs monthly partitions (6M rows)
	docker compose run --rm go go run ./cmd/partition
twosys: up     ## 10: orders and payments in two databases: dual write vs 2PC vs saga vs outbox
	docker compose run --rm go go run ./cmd/twosys
livemigrate: up ## 11: schema changes on a 5M-row table under load: blocking vs online
	docker compose run --rm go go run ./cmd/livemigrate
engines:       ## 12: the same workload on a B+tree (bbolt) and an LSM tree (Pebble)
	docker compose run --rm go go run ./cmd/engines
acid:          ## 13: transfers in PostgreSQL, MongoDB and DynamoDB Local, with and without transactions
	docker compose --profile acid up -d --wait postgres mongors dynamodb
	docker compose run --rm go go run ./cmd/acid
acid-test:     ## the ACID test (needs the acid profile)
	docker compose --profile acid up -d --wait postgres mongors dynamodb
	docker compose run --rm go go test -count=1 ./internal/acid/
pgdyn:         ## 14: the same orders in PostgreSQL and a DynamoDB single-table design
	docker compose --profile acid up -d --wait postgres dynamodb
	docker compose run --rm go go run ./cmd/pgdyn
pgdyn-test:    ## the pgdyn test (needs DynamoDB Local)
	docker compose --profile acid up -d --wait postgres dynamodb
	docker compose run --rm go go test -count=1 ./internal/pgdyn/
dr:            ## 15: backups and a disaster drill (logical dump vs base backup + WAL archive)
	./15-backup-restore/run.sh
replicas-test: ## the replica test (needs the replicas profile running)
	docker compose --profile replicas up -d --wait pgprimary pgreplica
	docker compose run --rm -e PRIMARY_URL=postgres://lab:lab@pgprimary:5432/lab?sslmode=disable -e REPLICA_URL=postgres://lab:lab@pgreplica:5432/lab?sslmode=disable go go test -count=1 ./internal/replicas/
test: up       ## run the tests that pin every result
	docker compose up -d --wait mysql
	docker compose run --rm go go test -race -count=1 -p 1 ./...
	docker compose run --rm go go test -count=1 ./internal/engines/
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node --test'
