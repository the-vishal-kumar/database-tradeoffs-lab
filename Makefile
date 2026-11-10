.PHONY: up down isolation test
up:            ## start the databases
	docker compose up -d --wait postgres
down:          ## stop and remove everything
	docker compose down -v
isolation: up  ## 01: the isolation-level matrix, with details
	docker compose run --rm go go run ./cmd/isolation -details
test: up       ## run the tests that pin the matrix
	docker compose run --rm go go test -race -count=1 ./...
