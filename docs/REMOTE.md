# Сетевой режим: один сервер на все устройства

Тот же бинарник умеет работать двумя способами:

- **stdio** — Claude запускает сервер подпроцессом на вашей машине. Просто, но работает только там, где стоит бинарник.
- **Streamable HTTP** — сервер живёт на VPS, к нему подключаются ноутбук, рабочая машина и телефон. Ключ Ozon при этом лежит в одном месте, а не копируется на каждое устройство.

## Развёртывание на VPS

### 1. Сборка и установка

```bash
git clone https://github.com/MainAlexStark/ozon-seller-mcp
cd ozon-seller-mcp
go build -ldflags "-s -w -X main.version=$(git describe --tags --always)" \
  -o /usr/local/bin/ozon-seller-mcp ./cmd/ozon-seller-mcp
```

### 2. Пароль владельца

```bash
ozon-seller-mcp --hash-password
```

Пароль читается со stdin: аргументы командной строки видны через `ps` и остаются в истории оболочки. Сам пароль нигде не хранится — на сервере только PBKDF2-хеш.

### 3. Конфигурация

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ozonmcp
sudo cp deploy/ozon-seller-mcp.env /etc/ozon-seller-mcp.env
sudo chmod 600 /etc/ozon-seller-mcp.env
sudo nano /etc/ozon-seller-mcp.env      # ключ Ozon, внешний адрес, хеш пароля
```

Секреты лежат в файле, а не в строке запуска: аргументы процесса видны любому пользователю через `ps`.

### 4. Служба

```bash
sudo cp deploy/ozon-seller-mcp.service /etc/systemd/system/
sudo systemctl enable --now ozon-seller-mcp
sudo systemctl status ozon-seller-mcp
```

Юнит слушает `127.0.0.1:8571` — наружу порт не выставлен.

### 5. TLS через Caddy

```bash
sudo apt install -y caddy
```

```bash
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo nano /etc/caddy/Caddyfile          # заменить домен
sudo systemctl reload caddy
```

Сертификат Caddy выпустит и будет продлевать сам.

В конфиге вычищается строка запроса: секретов в адресе больше нет, но туда может попасть код авторизации, если клиент ошибётся с методом.

### 6. Проверка

```bash
curl https://ozon-mcp.example.com/healthz
# {"status":"ok"}

# 401 с указанием, где искать метаданные — так Claude узнаёт про OAuth
curl -si -X POST https://ozon-mcp.example.com/mcp -d '{}' | grep -i www-authenticate

curl -s https://ozon-mcp.example.com/.well-known/oauth-protected-resource
```

Поле `resource` обязано совпадать с адресом, который вы введёте в Claude, посимвольно.

## Подключение устройств

Везде один и тот же адрес, без секретов:

```
https://ozon-mcp.example.com/mcp
```

Откроется экран согласия: вводите пароль владельца и выбираете права для этого устройства. Подробности и разбор решений — [`docs/OAUTH.md`](OAUTH.md).

Управление доступом:

```bash
ozon-seller-mcp --grants           # кто подключён
ozon-seller-mcp --revoke <выдача>  # отключить одно устройство
```

## Docker

```bash
docker build -t ozon-seller-mcp .
docker run -d --name ozon-mcp \
  -p 127.0.0.1:8571:8571 \
  --env-file /etc/ozon-seller-mcp.env \
  ozon-seller-mcp
```

Публикация порта тоже только на петлю: TLS остаётся за обратным прокси.

## Что ещё стоит включить

**Ограничение по адресам.** Запросы приходят с серверов Anthropic. Если вы не пользуетесь сервером через `curl`, можно пустить на `/mcp` только их диапазоны — тогда утёкший токен станет бесполезен для постороннего. Диапазоны публикует Anthropic; при их изменении подключение отвалится, поэтому взвесьте.

**Мониторинг отказов.** В журнале видны строки «отказ в доступе» с адресом источника. Всплеск таких строк означает, что адрес вашего сервера кто-то нашёл и перебирает токены.

```bash
journalctl -u ozon-seller-mcp -f | grep "отказ в доступе"
```

## Устройство транспорта

Сервер без состояния: `Mcp-Session-Id` не выдаётся и не требуется. Каждый запрос самодостаточен, поэтому переживает перезапуск процесса и не сломается за балансировщиком, если однажды понадобится второй экземпляр.

- `POST /mcp` — JSON-RPC внутрь, JSON наружу.
- `GET /mcp` и `DELETE /mcp` — `405`: сервер сам ничего не инициирует, а сессий, которые можно было бы удалить, нет.
- Уведомления (сообщения без `id`) — `202` без тела.
- Заголовок `Origin` проверяется: сайт в браузере не должен ходить к серверу от вашего имени.

Оба транспорта — stdio и HTTP — используют одну реализацию протокола, [официальный Go SDK](https://github.com/modelcontextprotocol/go-sdk). Расхождение между ними означало бы разное поведение сервера в зависимости от способа подключения, а такое не отлаживается.

### Заголовки для своих скриптов

Клиенты Claude ставят их сами, `curl` — нет. Спецификация требует от клиента готовности принять оба формата ответа, и запрос без этого отклоняется с `400`:

```bash
curl -sS https://example.com/mcp \
  -H "Authorization: Bearer $OZON_TOKEN_READ" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Отвечает сервер обычным JSON: потоком событий он не пользуется, потому что сам ничего не инициирует.
