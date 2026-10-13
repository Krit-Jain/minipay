.PHONY: vet build test migrate run

vet:
	go vet ./...

build:
	go build ./...

test: vet
	go test ./... -count=1

migrate:
	@test -n "$(DATABASE_URL)" || (echo "DATABASE_URL is not set" && exit 1)
	psql "$(DATABASE_URL)" -f migrations/0001_init.sql

run:
	go run ./cmd/api
