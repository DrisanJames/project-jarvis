.PHONY: build test lint migrate dev docker-build clean

BINARY=server
GO_FLAGS=-p 2

build:
	CGO_ENABLED=0 go build $(GO_FLAGS) -o bin/$(BINARY) ./cmd/server/

test:
	go test -race -count=1 ./internal/...

lint:
	golangci-lint run ./...

migrate:
	go run ./cmd/migrate/ migrations/

dev:
	go run ./cmd/server/

docker-build:
	docker build -t ignite-upside-down .

clean:
	rm -rf bin/
