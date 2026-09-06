.PHONY: build test check install run fmt

build:
	go build -o bin/ozon-seller-mcp ./cmd/ozon-seller-mcp

install:
	go install ./cmd/ozon-seller-mcp

test:
	go test ./...

fmt:
	gofmt -w .

check: fmt
	go vet ./...
	go test -race ./...

# Проверить ключи, не запуская сервер.
verify:
	go run ./cmd/ozon-seller-mcp --check
