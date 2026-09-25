.PHONY: build test test-db check install run fmt deploy update

build:
	go build -o bin/ozon-seller-mcp ./cmd/ozon-seller-mcp

install:
	go install ./cmd/ozon-seller-mcp

test:
	go test ./...

# Тесты вместе с хранилищем и кабинетом — против настоящего PostgreSQL.
test-db:
	docker rm -f ozon-mcp-testdb >/dev/null 2>&1 || true
	docker run --rm -d --name ozon-mcp-testdb -p 55432:5432 -e POSTGRES_PASSWORD=test postgres:16-alpine >/dev/null
	until docker exec ozon-mcp-testdb pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	OZON_TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres?sslmode=disable go test -race -count=1 ./... ; \
	  status=$$?; docker rm -f ozon-mcp-testdb >/dev/null; exit $$status

fmt:
	gofmt -w .

check: fmt
	go vet ./...
	go test -race ./...

# Проверить ключи из окружения (stdio), не запуская сервер.
verify:
	go run ./cmd/ozon-seller-mcp --check

# Развернуть на VPS: make deploy DOMAIN=mcp.example.com
deploy:
	./deploy.sh $(DOMAIN)

# Обновить развёрнутый сервер до свежего коммита.
update:
	./deploy.sh update
