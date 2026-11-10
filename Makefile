.PHONY: up down isolation slowquery mongo test
up:            ## start the databases
	docker compose up -d --wait postgres mongo
down:          ## stop and remove everything
	docker compose down -v
isolation: up  ## 01: the isolation-level matrix, with details
	docker compose run --rm go go run ./cmd/isolation -details
slowquery: up  ## 02: why queries slow down as the table grows (10K, 1M, 10M rows; several minutes)
	docker compose run --rm go go run ./cmd/slowquery -sizes 10000,1000000,10000000
mongo: up      ## 03: Mongoose vs $$jsonSchema vs one collection for everything
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node demo.mjs'
test: up       ## run the tests that pin every result
	docker compose run --rm go go test -race -count=1 -p 1 ./...
	docker compose run --rm node sh -c 'npm ci --no-audit --no-fund && node --test'
