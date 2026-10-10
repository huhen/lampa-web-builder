# lampa-web-builder

Сборщик веб-фронта [Lampa](https://github.com/yumata/lampa-source) в виде
Docker-образа + минимальный API сборки.

Что делает: качает upstream по коммиту, применяет наши патчи (`patches/`) и
overlay (`overlay/`), подставляет плейсхолдеры (домен), собирает gulp'ом и
отдаёт результат архивом `tar.gz` (содержимое `build/github/lampa/` в корне
архива).

## API

Все методы, кроме `/healthz`, требуют заголовок `X-API-Key`.

| Метод и путь                      | Назначение                                                                       |
|-----------------------------------|----------------------------------------------------------------------------------|
| `GET /healthz`                    | liveness                                                                         |
| `GET /api/v1/status`              | `available_commit`, `latest_seen_commit`, `bad_commit`, poll-инфо                |
| `POST /api/v1/check`              | разовая проверка upstream (+ тестовая сборка при новом коммите)                  |
| `POST /api/v1/builds`             | заказать сборку: `{"domain": "lampa.example.com"}` → `202 {"build_id","cached"}` |
| `GET /api/v1/builds/{id}`         | статус сборки (клиент пуллит)                                                    |
| `GET /api/v1/builds/{id}/archive` | архив (`status=success` и не выселенный из кэша)                                 |
| `GET /api/v1/builds/{id}/logs`    | полный лог сборки                                                                |

Коммит не параметризуется — всегда собирается последний доступный (`available_commit`).
Ошибки заказа: `400` (домен не `^[a-z0-9.-]+$`), `409`(идёт тестовая сборка), `413` (тело больше 64 КиБ), `429` (очередь полна),`503` (ещё нет доступной версии).

## Конфигурация (env)

| Переменная        | По умолчанию                                 | Значение                                                                                                                                         |
|-------------------|----------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|
| `LISTEN`          | `:8080`                                      | адрес HTTP-сервера                                                                                                                               |
| `API_KEY`         | — (обязателен)                               | ключ доступа                                                                                                                                     |
| `UPSTREAM_REPO`   | `https://github.com/yumata/lampa-source.git` | upstream                                                                                                                                         |
| `UPSTREAM_BRANCH` | `main`                                       | ветка upstream                                                                                                                                   |
| `DEFAULT_DOMAIN`  | — (обязателен)                               | домен тестовых сборок                                                                                                                            |
| `POLL_INTERVAL`   | `0` (off)                                    | интервал опроса upstream (`10m`, `6h`…; `0` = выключено)                                                                                         |
| `DATA_DIR`        | `/data`                                      | каталог состояния, сборок и рабочих копий                                                                                                        |
| `ASSETS_DIR`      | `.` (в образе `/app`)                        | где лежат `patches/`, `overlay/`, `package-lock.json`                                                                                            |
| `CACHE_SIZE`      | `10`                                         | размер кэша сборок (ёмкость очереди = `CACHE_SIZE + 1`); эффективное значение ограничено `50` (`MaxHistory`) — кэш и очередь размеруются от него |

## Запуск

```yaml
services:
  builder:
    image: ghcr.io/huhen/lampa-web-builder:latest
    restart: unless-stopped
    networks: [default]
    volumes:
      - builder-data:/data
    environment:
      API_KEY: ${BUILDER_API_KEY}
      DEFAULT_DOMAIN: ${BUILDER_DEFAULT_DOMAIN}
      POLL_INTERVAL: "6h"
    # ports нет: доступен только внутри сети compose по имени `builder`
volumes:
  builder-data:
```

Образ работает от непривилегированного пользователя `builder` (uid/gid 10001).
Named volume при создании наследует владельца из образа — дополнительных
действий не нужно. Для **bind mount** каталог на хосте должен принадлежать
`10001:10001` (`chown -R 10001:10001 <dir>`).

Volume, созданный прежним root-образом, нужно один раз перевести на нового
владельца. Изнутри контейнера под `builder` это невозможно, поэтому —
helper-контейнером от root:

```sh
docker run --rm -u 0 -v builder-data:/data alpine chown -R 10001:10001 /data
```

Compose префиксует имя volume именем проекта (`docker volume ls` покажет
фактическое, обычно `<проект>_builder-data`) — в команде подставьте его.

Переходный вариант, пока lampa-go не контейнеризирован: опубликовать
`127.0.0.1:8081:8080` и ходить с хоста на `http://127.0.0.1:8081`.

Каждый мерж в `main` публикует образ: `main` (последняя сборка ветки) и
`sha-<sha>` (закреплена за коммитом). Теги `v*` публикуют релизные версии
и `latest`.

## Разработка

```sh
make build        # собрать бинарник в bin/
make test         # go test ./...
make vet          # go vet ./...
make docker-build # собрать образ
make e2e          # полная сборка с настоящими npm/gulp (сеть, минуты)
```

Сборки в контейнере требуют `git`, `node`, `npm` — они в образе; для локального
`make run` они должны быть на хосте.

## Лицензия

Код билдера (Go-сервер, `patches/`, `overlay/`) — [GPL-2.0](LICENSE).
Собранный фронтенд — upstream [lampa-source](https://github.com/yumata/lampa-source)
под GPL-2.0, копия лицензии попадает в каждый архив сборки.
