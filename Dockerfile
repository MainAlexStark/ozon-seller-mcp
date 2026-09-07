# Сборка
FROM golang:1.25-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: он переживает правки кода и не тянет
# модули заново на каждую сборку.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker

RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ozon-seller-mcp ./cmd/ozon-seller-mcp

# Пустой каталог под хранилище OAuth. Он создаётся здесь, а не в
# compose, ради прав: том Docker наследует владельца от каталога
# в образе. Без этого сервер под nonroot не смог бы создать в нём
# oauth.json, и запуск падал бы на «хранилище OAuth: permission denied».
RUN mkdir -p /data


# Запуск
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ozon-seller-mcp /ozon-seller-mcp

# 65532 — uid nonroot в distroless. Числом, а не именем: имя пришлось бы
# разрешать через /etc/passwd целевого образа.
COPY --from=build --chown=65532:65532 /data /var/lib/ozon-seller-mcp

USER nonroot:nonroot

EXPOSE 8571

ENTRYPOINT ["/ozon-seller-mcp"]
CMD ["--http", "0.0.0.0:8571"]
