.PHONY: vet build test migrate run

vet:
	go vet ./...

build:
	go build ./...

test: vet
	go test ./... -count=1

migrate:
	@test -n "$(DATABASE_URL)" || (echo "DATABASE_URL is not set" && exit 1)
	for f in migrations/*.sql; do psql "$(DATABASE_URL)" -f "$$f" || exit 1; done

run:
	go run ./cmd/api
