.PHONY: up down isolation slowquery test
up:            ## start the databases
	docker compose up -d --wait postgres
down:          ## stop and remove everything
	docker compose down -v
isolation: up  ## 01: the isolation-level matrix, with details
	docker compose run --rm go go run ./cmd/isolation -details
slowquery: up  ## 02: why queries slow down as the table grows (10K, 1M, 10M rows; several minutes)
	docker compose run --rm go go run ./cmd/slowquery -sizes 10000,1000000,10000000
test: up       ## run the tests that pin every result
	docker compose run --rm go go test -race -count=1 ./...
