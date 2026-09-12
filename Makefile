.PHONY: build test check install run fmt deploy update

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

# Развернуть на VPS: make deploy DOMAIN=mcp.example.com
deploy:
	./deploy.sh $(DOMAIN)

# Обновить развёрнутый сервер до свежего коммита.
update:
	./deploy.sh update
