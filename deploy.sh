#!/usr/bin/env bash
#
# Развёртывание ozon-seller-mcp на VPS и его обновление.
#
#   ./deploy.sh ozon-mcp.example.com    развернуть
#   ./deploy.sh update                  обновить
#
# Скрипт делает всё, что раньше приходилось делать руками: заводит сеть
# docker, поднимает общий Caddy, вписывает домен в его Caddyfile,
# придумывает пароль владельца и токены, поднимает сервер и дожидается,
# пока тот ответит по HTTPS. Повторный запуск ничего не ломает: пароль
# и токены не перевыпускаются, блок в Caddyfile заменяется на месте.

APP="ozon-seller-mcp"
PREFIX="OZON"
PORT="8571"
APP_DNS_HINT="(например ozon-mcp.example.com)"

USAGE_EXTRA='
Ключи Ozon спрашиваются один раз. Без диалога:
  OZON_CLIENT_ID=… OZON_API_KEY=… ./deploy.sh ozon-mcp.example.com'

# prepare_extra — то, без чего именно этот сервер не поднимется.
prepare_extra() {
  ask_secret OZON_CLIENT_ID "Client-Id (Настройки → API-ключи в кабинете продавца)"
  ask_secret OZON_API_KEY   "API-ключ"
}

# ── общая часть: одинакова во всех MCP-серверах стандарта ────────────

set -euo pipefail

cd "$(dirname "$0")"

ENV_FILE="deploy/${APP}.env"
INFRA_DIR="/opt/infrastructure"
CADDYFILE="${INFRA_DIR}/Caddyfile"

# Docker почти всегда требует root. Разбираемся один раз здесь, чтобы
# дальше не гадать: без этого половина команд молча падает на правах.
SUDO=""
if [ "$(id -u)" != "0" ] && ! docker info >/dev/null 2>&1; then
  SUDO="sudo"
fi

die()  { printf '\n%s: %s\n' "$APP" "$1" >&2; exit 1; }
say()  { printf '%s\n' "$1"; }
step() { printf '\n== %s\n' "$1"; }

# gen_secret печатает случайную строку из букв и цифр.
#
# Без символов пунктуации намеренно: значение уходит в env-файл, откуда
# его читает docker compose, и кавычки с долларами там ведут себя
# по-разному в зависимости от версии.
gen_secret() {
  local n="${1:-32}"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$n" | cut -c "1-$((n * 2))"
  else
    LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c "$((n * 2))"
  fi
}

# env_get читает значение из env-файла, env_set — записывает,
# не трогая остальные строки и комментарии.
env_get() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -1
}

env_set() {
  local key="$1" value="$2"
  touch "$ENV_FILE"
  if grep -q "^${key}=" "$ENV_FILE"; then
    # Значение может содержать слэши, поэтому разделитель не /.
    sed -i "s|^${key}=.*|${key}=${value}|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
  fi
}

require_tools() {
  command -v docker >/dev/null 2>&1 || die \
    "docker не установлен. На Debian/Ubuntu: curl -fsSL https://get.docker.com | sh"
  $SUDO docker compose version >/dev/null 2>&1 || die \
    "нет плагина docker compose (docker-compose-plugin)"
  command -v curl >/dev/null 2>&1 || die "нужен curl"
  command -v git  >/dev/null 2>&1 || die "нужен git"
}

ensure_network() {
  if ! $SUDO docker network inspect mcp-network >/dev/null 2>&1; then
    say "создаю сеть mcp-network"
    $SUDO docker network create mcp-network >/dev/null
  fi
}

# ensure_caddy поднимает общий обратный прокси, если его ещё нет.
#
# Он общий на всю машину намеренно: порты 80 и 443 одни, а MCP-серверов
# на VPS обычно несколько. Каждый вписывает в его Caddyfile свой блок
# и ходит к нему по имени контейнера внутри mcp-network.
ensure_caddy() {
  if [ ! -f "${INFRA_DIR}/docker-compose.yml" ]; then
    say "поднимаю общий Caddy в ${INFRA_DIR}"
    $SUDO mkdir -p "$INFRA_DIR"
    $SUDO tee "${INFRA_DIR}/docker-compose.yml" >/dev/null <<'CADDY_COMPOSE'
# Общий обратный прокси для всех MCP-серверов этой машины.
# Создан автоматически скриптом deploy.sh одного из них.
services:
  caddy:
    image: caddy:2-alpine
    container_name: caddy
    restart: unless-stopped

    ports:
      - "80:80"
      - "443:443"

    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config

    networks:
      - mcp-network

    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

networks:
  mcp-network:
    external: true

volumes:
  caddy-data:
  caddy-config:
CADDY_COMPOSE
  fi

  [ -f "$CADDYFILE" ] || $SUDO tee "$CADDYFILE" >/dev/null <<'CADDYFILE_HEAD'
# Общий Caddyfile MCP-серверов. Блоки между маркерами
# «# >>> имя» и «# <<< имя» пишет deploy.sh соответствующего сервера —
# правьте их там, иначе следующее развёртывание перезапишет правку.
CADDYFILE_HEAD
}

# caddy_vhost вписывает блок этого сервера, заменяя прежний.
#
# По маркерам, а не поиском по домену: домен может смениться, и тогда
# без маркеров в файле осталось бы два блока, из которых работал бы
# первый — а чинить это пришлось бы в три часа ночи.
caddy_vhost() {
  local tmp
  tmp="$(mktemp)"

  if [ -f "$CADDYFILE" ]; then
    $SUDO awk -v app="$APP" '
      $0 == "# >>> " app { skip = 1 }
      skip != 1 { print }
      $0 == "# <<< " app { skip = 0 }
    ' "$CADDYFILE" > "$tmp"
  fi

  {
    printf '# >>> %s\n' "$APP"
    printf '%s {\n' "$DOMAIN"
    printf '    reverse_proxy %s:%s {\n' "$APP" "$PORT"
    printf '        transport http {\n'
    # Инструменты бывают долгими: ожидание выката, импорт товаров.
    # Со стандартным таймаутом прокси обрывает такой вызов раньше,
    # чем сервер успевает ответить, и это выглядит как поломка сервера.
    printf '            response_header_timeout 6m\n'
    printf '        }\n'
    printf '    }\n'
    printf '}\n'
    printf '# <<< %s\n' "$APP"
  } >> "$tmp"

  $SUDO cp "$tmp" "$CADDYFILE"
  rm -f "$tmp"

  $SUDO docker compose -f "${INFRA_DIR}/docker-compose.yml" up -d >/dev/null
  # reload вместо restart: перезапуск прокси рвёт соединения соседних
  # серверов, которые к этому развёртыванию отношения не имеют.
  $SUDO docker exec caddy caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1 \
    || $SUDO docker restart caddy >/dev/null
}

# ask_secret спрашивает значение, которого нет в env-файле.
ask_secret() {
  local key="$1" prompt="$2" current value
  current="$(env_get "$key")"
  [ -n "$current" ] && return 0

  # Значение могли передать окружением: так разворачивают без диалога.
  value="$(printenv "$key" || true)"
  if [ -z "$value" ]; then
    [ -t 0 ] || die "не задан $key. Передайте его окружением: $key=… ./deploy.sh $DOMAIN"
    printf '%s: ' "$prompt"
    read -r value
  fi
  [ -n "$value" ] || die "$key не может быть пустым"
  env_set "$key" "$value"
}

wait_health() {
  local i code
  printf 'жду ответа по https://%s/healthz ' "$DOMAIN"
  for i in $(seq 1 45); do
    code="$(curl -fsS -m 5 -o /dev/null -w '%{http_code}' "https://${DOMAIN}/healthz" 2>/dev/null || true)"
    if [ "$code" = "200" ]; then
      printf ' есть\n'
      return 0
    fi
    printf '.'
    sleep 2
  done
  printf ' нет\n'
  return 1
}

# compose_up собирает и поднимает сервер.
#
# Версия берётся из git и попадает в бинарник через -ldflags, поэтому
# --version на развёрнутом сервере отвечает номером коммита, а не «dev»:
# первый вопрос при разборе «а что там вообще стоит» — именно этот.
compose_up() {
  local version
  version="$(git describe --tags --always --dirty 2>/dev/null || echo docker)"
  if [ -n "$SUDO" ]; then
    sudo VERSION="$version" docker compose up -d --build
  else
    VERSION="$version" docker compose up -d --build
  fi
}

print_secrets() {
  cat <<SECRETS

  Адрес для Claude:  https://${DOMAIN}/mcp
  Пароль владельца:  $(env_get "${PREFIX}_OWNER_PASSWORD")
  Токен на чтение:   $(env_get "${PREFIX}_TOKEN_READ")
  Токен на запись:   $(env_get "${PREFIX}_TOKEN_WRITE")

Пароль вводится на экране согласия при подключении устройства.
Токены нужны только автоматизации, которая не может пройти этот экран.
Всё это лежит в ${ENV_FILE} (права 600) и печатается снова по
  ./deploy.sh secrets
SECRETS
}

cmd_install() {
  require_tools

  step "окружение"
  [ -f "$ENV_FILE" ] || {
    mkdir -p deploy
    cp "deploy/${APP}.env.example" "$ENV_FILE"
    say "создан $ENV_FILE из примера"
  }
  chmod 600 "$ENV_FILE"

  env_set "${PREFIX}_PUBLIC_URL" "https://${DOMAIN}"
  env_set "${PREFIX}_HTTP_ADDR" ""

  # Пароль и токены придумываются один раз и потом не трогаются:
  # перевыпуск отключил бы уже подключённые устройства.
  [ -n "$(env_get "${PREFIX}_OWNER_PASSWORD")" ] || {
    env_set "${PREFIX}_OWNER_PASSWORD" "$(gen_secret 12)"
    say "придуман пароль владельца"
  }
  [ -n "$(env_get "${PREFIX}_TOKEN_READ")" ]  || env_set "${PREFIX}_TOKEN_READ"  "$(gen_secret 32)"
  [ -n "$(env_get "${PREFIX}_TOKEN_WRITE")" ] || env_set "${PREFIX}_TOKEN_WRITE" "$(gen_secret 32)"

  # Хеш пароля больше не нужен: сервер считает его сам из открытого
  # пароля. Старую строку убираем, иначе она молча победит новый пароль.
  env_set "${PREFIX}_OWNER_PASSWORD_HASH" ""

  prepare_extra

  step "инфраструктура"
  ensure_network
  ensure_caddy
  caddy_vhost
  say "домен ${DOMAIN} → ${APP}:${PORT}"

  step "сервер"
  compose_up

  step "проверка"
  if ! wait_health; then
    say ""
    say "Сервер не ответил. Частые причины, по убыванию:"
    say "  1. DNS: ${DOMAIN} ещё не указывает на этот VPS (проверьте: dig +short ${DOMAIN})"
    say "  2. порты 80 и 443 закрыты — Let's Encrypt не смог выдать сертификат"
    say "  3. сервер упал на старте — смотрите ./deploy.sh logs"
    exit 1
  fi

  print_secrets
}

cmd_update() {
  require_tools
  [ -f "$ENV_FILE" ] || die "сервер здесь не развёрнут: сначала ./deploy.sh <домен>"

  DOMAIN="$(env_get "${PREFIX}_PUBLIC_URL" | sed 's#^https\?://##')"
  [ -n "$DOMAIN" ] || die "в $ENV_FILE не задан ${PREFIX}_PUBLIC_URL"

  local before
  before="$(git rev-parse HEAD)"

  step "обновление исходников"
  git pull --ff-only

  if [ "$(git rev-parse HEAD)" = "$before" ]; then
    say "уже последняя версия — пересобираю на всякий случай"
  fi

  step "пересборка"
  compose_up

  step "проверка"
  if wait_health; then
    say ""
    say "Обновлено до $(git rev-parse --short HEAD): $(git log -1 --pretty=%s)"
    return 0
  fi

  # Откат — обязательная часть обновления, а не удобство: без него
  # неудачный git pull оставляет сервер лежать до ручного разбора,
  # и узнаёте вы об этом от того, кто им пользуется.
  say ""
  say "Новая версия не отвечает — откатываюсь на ${before:0:12}"
  git reset --hard "$before" >/dev/null
  compose_up
  if wait_health; then
    die "обновление не удалось, вернулся прежний сервер. Смотрите ./deploy.sh logs"
  fi
  die "не отвечает и прежняя версия — дело не в коде. Смотрите ./deploy.sh logs"
}

cmd_status() {
  $SUDO docker compose ps
  DOMAIN="$(env_get "${PREFIX}_PUBLIC_URL" | sed 's#^https\?://##')"
  if [ -n "$DOMAIN" ]; then
    printf '\nhttps://%s/healthz → %s\n' "$DOMAIN" \
      "$(curl -fsS -m 5 -o /dev/null -w '%{http_code}' "https://${DOMAIN}/healthz" 2>/dev/null || echo "нет ответа")"
  fi
}

cmd_logs()    { $SUDO docker compose logs -f --tail 200; }
cmd_restart() { $SUDO docker compose restart; }
cmd_down()    { $SUDO docker compose down; }

cmd_secrets() {
  [ -f "$ENV_FILE" ] || die "сервер здесь не развёрнут"
  DOMAIN="$(env_get "${PREFIX}_PUBLIC_URL" | sed 's#^https\?://##')"
  print_secrets
}

usage() {
  cat <<USAGE
${APP} — развёртывание на VPS

  ./deploy.sh <домен>     развернуть или перенастроить на этот домен
  ./deploy.sh update      обновить до свежего коммита (с откатом при неудаче)
  ./deploy.sh status      что запущено и отвечает ли сервер
  ./deploy.sh logs        журнал сервера
  ./deploy.sh secrets     показать пароль владельца и токены
  ./deploy.sh restart     перезапустить
  ./deploy.sh down        остановить

Перед первым запуском заведите A-запись ${APP_DNS_HINT} на адрес этого VPS.
${USAGE_EXTRA}
USAGE
}

case "${1:-}" in
  ""|-h|--help|help) usage ;;
  update)  cmd_update ;;
  status)  cmd_status ;;
  logs)    cmd_logs ;;
  secrets) cmd_secrets ;;
  restart) cmd_restart ;;
  down)    cmd_down ;;
  *)
    case "$1" in
      *.*) DOMAIN="$1" ;;
      *)   die "«$1» не похоже на домен и не является командой. ./deploy.sh --help" ;;
    esac
    cmd_install
    ;;
esac
