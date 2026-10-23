# BVK Support

Telegram-бот технической поддержки: клиент пишет боту в личный чат, инженеры
работают в закрытых форум-группах настроенных отделов (сейчас IT, 1С, Выезд). Вся
логика и состояние — в PostgreSQL; отдельный процесс строит витрину `analytics`
для Metabase. Опционально бот принимает заявки из общих Telegram-чатов компаний
по тегам (`#1с`, `#it`, `#выезд`): переписка остаётся в чате, отдельный
топик не создаётся. Канал включается настройкой `client_groups`.

**Стек:** Go 1.26.9, PostgreSQL, `pgx/v5` 5.9.2, Bot API напрямую — без Telegram
SDK, ORM, Redis и брокера. Часовой пояс бизнес-логики — Asia/Irkutsk, рабочий
календарь Пн–Пт 09:00–18:00.

## Быстрый старт

**Для текущего тестового окружения нового Telegram-бота** используйте
[`docs/PILOT-DB-SETUP.ru.md`](docs/PILOT-DB-SETUP.ru.md) и `./scripts/pilot.sh`.
Корневой `config.json` сейчас содержит настройки пилота. Приватный файл БД,
указанный в `database_url_file`, имеет приоритет над старым `DATABASE_URL` в терминале.

Текущий пилот:

```sh
go build -trimpath -o bin/bvk ./cmd/bvk
export TELEGRAM_BOT_TOKEN='…'
./bin/bvk -config config.json -check
./bin/bvk -config config.json
```

Для новой установки вне пилота:

```sh
cp config.example.json config.json      # заполнить admin_id, staff, отделы, компании
go run ./cmd/bvk-code -bot ИМЯ_БОТА    # выпустить код и ссылку для каждой компании
export DATABASE_URL='postgres://…'
export TELEGRAM_BOT_TOKEN='…'

go build -trimpath -o bin/bvk ./cmd/bvk
./bin/bvk -config config.json -migrate  # схема и bootstrap, токен не нужен
./bin/bvk -config config.json -check    # БД, токен, группы, права и темы очереди
./bin/bvk -config config.json           # рабочий режим
```

Копия с настоящим восстановлением во временную БД:

```sh
./bin/bvk -config config.json -backup
```

Аналитика — отдельный процесс, токен Telegram ей не нужен:

```sh
go build -trimpath -o bin/bvk-analytics ./cmd/bvk-analytics
./bin/bvk-analytics -config config.json
```

`bin/`, `data/` и `config.json` — локальные артефакты и в Git не входят.

## Тесты

```sh
go test ./...
```

Без `TEST_DATABASE_URL` тесты, работающие с PostgreSQL, **молча пропускаются** —
зелёный прогон в этом режиме не доказывает сохранность данных. Дополнительно:

| Переменная | Что включает |
|---|---|
| `TEST_DATABASE_URL` | тесты хранилища, миграций и сквозных сценариев |
| `TEST_BACKUP_TOOLS=1` | проверку `pg_dump`/`pg_restore` и восстановления копии |
| `TEST_TELEGRAM_LIVE=1` | живой сценарий Telegram (нужен `TEST_TELEGRAM_CONFIG`) |
| `TEST_PRERELEASE_LOAD=1` | нагрузочные прогоны на одноразовой схеме |
| `TEST_ANALYTICS_LOAD=1` | нагрузочный прогон аналитики |

## Документация

| Документ | Роль |
|---|---|
| [docs/STATE.md](docs/STATE.md) | **Единственный источник правды:** поведение, эксплуатация, аналитика, решения, дефекты и открытая работа |
| [docs/SYSTEM-OVERVIEW.ru.md](docs/SYSTEM-OVERVIEW.ru.md) | Практическая карта системы и руководство по копиям/восстановлению |
| [docs/AGENTS.md](docs/AGENTS.md) | Правила работы над проектом |
| [docs/AUDIT-2026-10-18.md](docs/AUDIT-2026-10-18.md) | Исторический отчёт предрелизного аудита |
| [archive/README.md](archive/README.md) | Архив удалённых Git-веток и спасённый патч |

## Структура

```
cmd/bvk/                 бот: флаги, старт, singleton, health
cmd/bvk-analytics/       расчётчик витрины analytics
internal/config/         JSON-конфиг, секреты из окружения, валидация
internal/domain/         модели, ошибки, интерфейсы хранилища
internal/groupintake/    парсер тегов групп компаний
internal/tickets/        жизненный цикл заявки, групповой канал, календарь, отчёты
internal/postgres/       транзакции, inbox/outbox, миграции, карантин
internal/telegram/       Bot API, маршрутизация, экраны, групповой чат, воркер доставки
internal/monitor/        напоминания по активным заявкам
internal/archive/        pg_dump/pg_restore, файлы, манифест
internal/analytics/      расчёт и публикация снимка
internal/report/         CSV для /report
internal/metrics/        гистограммы этапов
analytics/metabase/      manifest.json, SQL-карточки, импорт
scripts/                 сборка, службы Windows, аналитика
docs/                    документация
archive/                 исторический Git bundle, не рабочий код
```
