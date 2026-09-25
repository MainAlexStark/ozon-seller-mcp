# Сервис: один сервер на много пользователей

Тот же бинарник работает двумя способами:

- **stdio** — Claude запускает сервер подпроцессом на вашей машине. Один магазин, ключ в окружении. Для личного использования и разработки.
- **сервис** (`--http`) — сервер на VPS с PostgreSQL. Пользователи регистрируются сами, подключают свои магазины в кабинете и добавляют сервер в Claude по одному адресу. Каждое подключение Claude привязано к одному магазину одного пользователя.

## Что внутри

```
                    https://ozon-mcp.example.com
                               │
                             Caddy (TLS)
                               │
                        ozon-seller-mcp
   ┌───────────────┬───────────┴──────────┬──────────────────┐
   /  /signup       /oauth/*              /mcp               /healthz
   /login /account  регистрация клиента,  инструменты:       живость
   кабинет          согласие, токены      токен → пользователь
                                          → магазин → ключ Ozon
                               │
                          PostgreSQL
   пользователи · магазины (ключи зашифрованы) · сессии · OAuth · API-токены · учёт вызовов
```

На каждый запрос к `/mcp` сервер отвечает на три вопроса: чей токен, к какому магазину он выдан и с какими правами. Дальше в Ozon уходит запрос с ключом именно этого магазина, со своим лимитером: Ozon считает лимиты по Client-Id, и один активный продавец не тормозит остальных.

## Развёртывание: одна команда

Нужен VPS с Docker и A-запись домена на его адрес:

```
ozon-mcp.example.com    A    <адрес VPS>
```

```bash
git clone https://github.com/MainAlexStark/ozon-seller-mcp
cd ozon-seller-mcp
./deploy.sh ozon-mcp.example.com
```

Скрипт:

1. заведёт сеть `mcp-network` и поднимет общий Caddy в `/opt/infrastructure`, если его ещё нет;
2. впишет домен в его `Caddyfile` между маркерами `# >>> ozon-seller-mcp` и `# <<<`;
3. придумает пароль базы (`deploy/postgres.env`) и мастер-ключ шифрования (`OZON_SECRET_KEY` в `deploy/ozon-seller-mcp.env`), оба файла с правами 600;
4. соберёт образ и поднимет сервис вместе с PostgreSQL; миграции базы сервер применяет сам на старте;
5. дождётся `200` от `https://<домен>/healthz` — это значит, что поднялись и сервер, и база, и выпущен сертификат.

Повторный запуск ничего не ломает: секреты не перевыпускаются.

### Мастер-ключ

`OZON_SECRET_KEY` шифрует API-ключи магазинов в базе (AES-256-GCM). Два правила:

- **Не терять.** Без него зашифрованные ключи не прочитать — всем пользователям придётся подключать магазины заново. Сохраните копию в менеджере паролей: `./deploy.sh secrets`.
- **Не хранить рядом с дампами базы.** Дамп без ключа бесполезен; дамп вместе с ключом раскрывает ключи всех магазинов.

## Обновление: одна команда

```bash
./deploy.sh update
```

`git pull --ff-only`, недостающие секреты (существующие не трогаются), резервная копия базы в `backups/`, пересборка, ожидание `/healthz`. **Если новая версия не отвечает — скрипт откатывает код на прежний коммит.** Данные откатываются только вручную из копии: миграции схемы в этом проекте только добавляют, поэтому старый код на новой схеме обычно работает.

### Переход с однопользовательской версии

`./deploy.sh update` на сервере, развёрнутом до сервиса, сам заведёт базу и мастер-ключ и очистит пароль владельца и статические токены. Ключ Ozon из окружения больше не используется: зарегистрируйтесь на своём домене, подключите магазин в кабинете и переподключите сервер в Claude — старые подключения работать перестанут, а старое хранилище `oauth.json` не переносится.

## Остальные команды

```bash
./deploy.sh status     # что запущено и отвечает ли сервер
./deploy.sh logs       # журнал сервера
./deploy.sh backup     # дамп базы в backups/ (сжатый, права 600)
./deploy.sh users      # пользователи: магазины, вызовы за 30 дней, статус
./deploy.sh secrets    # адрес и мастер-ключ
./deploy.sh restart
./deploy.sh down
```

## Администрирование

Команды работают с той же базой, что и сервер:

```bash
sudo docker compose exec ozon-seller-mcp /ozon-seller-mcp --users
sudo docker compose exec ozon-seller-mcp /ozon-seller-mcp --disable user@example.com
sudo docker compose exec ozon-seller-mcp /ozon-seller-mcp --enable user@example.com
sudo docker compose exec -i ozon-seller-mcp /ozon-seller-mcp --set-password user@example.com
```

- `--disable` сразу закрывает кабинет, все подключения Claude и API-токены пользователя. Данные остаются.
- `--set-password` — для тех, кто забыл пароль (восстановления по почте пока нет). Пароль читается со stdin, все сессии кабинета закрываются.
- Регистрацию можно закрыть, не трогая существующих пользователей: `OZON_SIGNUP=closed` и `./deploy.sh restart`.

## Если сервер не ответил

1. **DNS.** `dig +short ozon-mcp.example.com` должен вернуть адрес этого VPS.
2. **Порты 80 и 443.** Без них Let's Encrypt не выдаст сертификат. `sudo docker compose -f /opt/infrastructure/docker-compose.yml logs caddy`.
3. **Сервер упал на старте.** `./deploy.sh logs`. Типичные причины: не задан или испорчен `OZON_SECRET_KEY`, база не поднялась (`sudo docker compose logs postgres`), неверный `OZON_DATABASE_URL`.

`/healthz` отвечает `503`, если сервер жив, а база нет.

## Вручную, под systemd

Для машины, где Docker не годится. PostgreSQL 14+ ставится отдельно.

```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags --always)" \
  -o /usr/local/bin/ozon-seller-mcp ./cmd/ozon-seller-mcp

sudo -u postgres createuser ozon --pwprompt
sudo -u postgres createdb -O ozon ozon

sudo useradd --system --no-create-home --shell /usr/sbin/nologin ozonmcp
sudo install -m 600 deploy/ozon-seller-mcp.env.example /etc/ozon-seller-mcp.env
ozon-seller-mcp --gen-secret             # → OZON_SECRET_KEY
sudo nano /etc/ozon-seller-mcp.env       # адрес, база, мастер-ключ

sudo cp deploy/ozon-seller-mcp.service /etc/systemd/system/
sudo systemctl enable --now ozon-seller-mcp
```

Юнит слушает `127.0.0.1:8571`, TLS — за обратным прокси. Проверка снаружи:

```bash
curl -s https://ozon-mcp.example.com/healthz
# {"status":"ok"}

# 401 с указанием, где искать метаданные — так Claude узнаёт про OAuth
curl -si -X POST https://ozon-mcp.example.com/mcp -d '{}' | grep -i www-authenticate

curl -s https://ozon-mcp.example.com/.well-known/oauth-protected-resource
```

Поле `resource` обязано совпадать с адресом, который пользователь вводит в Claude, посимвольно.

## Что ещё стоит включить

**Мониторинг отказов.** Строки «отказ в доступе» и «неудачный вход» в журнале с адресом источника. Всплеск — кто-то перебирает токены или пароли. Вход и регистрация ограничены десятью попытками подряд с одного адреса, дальше одна в 30 секунд.

**Резервные копии по расписанию.** `./deploy.sh backup` из cron раз в сутки и копирование `backups/` на другую машину.

## Устройство транспорта

Сервер без состояния: `Mcp-Session-Id` не выдаётся и не требуется. Каждый запрос самодостаточен, поэтому переживает перезапуск процесса и не сломается за балансировщиком.

- `POST /mcp` — JSON-RPC внутрь, JSON наружу. MCP живёт только на `/mcp`; корень и остальные пути — страницы сервиса.
- `GET /mcp` и `DELETE /mcp` — `405`.
- Уведомления (сообщения без `id`) — `202` без тела.
- Заголовок `Origin` на `/mcp` проверяется: сайт в браузере не должен ходить к серверу от имени пользователя.
- Токен к удалённому магазину — `403`, а не `401`: на `401` клиент бросился бы обновлять токен и зациклился.

### Свои скрипты: API-токены

Скрипт не может пройти экран согласия, поэтому для автоматизации в кабинете выпускаются персональные токены (`osm_…`): привязаны к одному магазину, только чтение или чтение и запись, бессрочные, отзываются кнопкой. Показываются один раз.

Клиенты Claude ставят заголовки сами, `curl` — нет. Спецификация требует готовности принять оба формата ответа, и запрос без этого отклоняется с `400`:

```bash
curl -sS https://ozon-mcp.example.com/mcp \
  -H "Authorization: Bearer $OSM_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```
