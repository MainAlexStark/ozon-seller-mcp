# Сборка
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ozon-seller-mcp ./cmd/ozon-seller-mcp

# Запуск
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ozon-seller-mcp /ozon-seller-mcp
USER nonroot:nonroot
EXPOSE 8571
ENTRYPOINT ["/ozon-seller-mcp"]
CMD ["--http", "0.0.0.0:8571"]
