# sa-hr-bot

Telegram-бот для тренировочных технических собеседований студентов курса по
системному анализу. Регистрация по коду доступа (`/start КОД`), дальше бот
сам ведёт кандидата по фазам собеседования: фиксированное вступление,
четыре вопроса квалификации, подтверждение профиля, урок КДИР, тематические
блоки из основного вопроса и двух уточнений, затем итоговый отчёт. Оценку
ответов формирует LLM по системному промпту из `prompts/`.
Администраторам (см. `ADMIN_IDS`) доступна команда `/report` — выгрузка
активности студентов.

Этот README описывает текущую реализованную версию. Подробная спецификация
сценария по PDF и фидбэку заказчиков записана в `workflow-new.md`. Видео урока
и заранее подготовленные живые контексты вопросов пока остаются следующими
задачами.

## Стек и обоснование выбора

**Telegram-библиотека: [telebot.v3](https://github.com/telebot/telebot)** (`gopkg.in/telebot.v3`).
Выбрана вместо `go-telegram-bot-api`, потому что:
- Даёт `Context`-ориентированный API (`c.Send(...)`, `c.Sender()` и т.д.),
  что удобно для диалогового сценария интервью с несколькими шагами —
  такой стиль ближе к веб-фреймворкам (echo/gin) и проще расширяется
  middleware/state-хендлерами по мере роста бизнес-логики.
- Встроенная поддержка long polling и webhook "из коробки" через единый
  интерфейс `Poller`.
- Активно поддерживается, простая маршрутизация команд (`bot.Handle("/start", ...)`).

**База данных: PostgreSQL** (через `github.com/jackc/pgx/v5`).
- Бот обрабатывает обновления от разных студентов параллельно (goroutine на
  апдейт), а PostgreSQL корректно обрабатывает конкурентную запись, в
  отличие от SQLite, где запись сериализуется на уровне файла.
- Нужны надёжные миграции (Goose) и стандартный SQL-тулинг для хранения
  сессий интервью и истории сообщений.
- Легко поднимается в Docker/Docker Compose локально и переносится на
  managed-инстанс (RDS, Cloud SQL и т.п.) в проде без смены драйвера.

**Миграции:** [Goose](https://github.com/pressly/goose) — SQL-файлы миграций
встраиваются в бинарник через `embed.FS` (`internal/migrations`) и
применяются автоматически при старте.

**LLM:** официальный [OpenAI Go SDK](https://github.com/openai/openai-go)
(`internal/llm`). Поддерживается `OPENAI_BASE_URL` для работы через прокси.

**Промпты:** хранятся как отдельные `.md`-файлы в `prompts/`, а не
хардкодятся в коде — это позволяет менять формулировки без пересборки
и код-ревью текста отдельно от логики.

## Структура проекта

```
cmd/bot/               точка входа бота (main.go)
cmd/import-questions/  утилита импорта question_bank.csv в БД
cmd/generate-codes/    утилита генерации кодов доступа
cmd/migrate/           применение Goose-миграций без запуска бота
internal/config/       загрузка конфигурации из окружения/.env
internal/handlers/     конечный автомат сессии + обработчики Telegram
internal/llm/          клиент и сервис OpenAI API
internal/db/           модели, репозиторий, подключение к БД (PostgreSQL/pgx)
internal/migrations/   SQL-миграции (Goose) + логика применения
prompts/                системный промпт в .md
media/                  картинки приветствия и старта тренажёра
```

## Запуск локально

### 1. Требования

- Go 1.25+ (см. `go.mod`). Если установлена более старая версия 1.21-1.24,
  `GOTOOLCHAIN=auto` (значение по умолчанию) сам скачает нужную при первой
  сборке.
- Запущенный PostgreSQL (локально или в Docker)

### 2. Конфигурация

```bash
cp .env.example .env
# заполните TELEGRAM_BOT_TOKEN, OPENAI_API_KEY, DATABASE_URL;
# ADMIN_IDS и SESSION_CYCLE_LIMIT опциональны (см. комментарии в .env.example)
```

### 3. Поднять PostgreSQL (пример через Docker)

```bash
docker run --name sa-hr-bot-db -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=sa_hr_bot -p 5432:5432 -d postgres:16-alpine
```

### 4. Сборка и запуск

```bash
go mod download
go build ./...
make migrate
make run
```

Миграции применяются автоматически при старте приложения (см.
`internal/migrations`). Отдельно от бота все миграции вверх запускаются командой
`make migrate`; для точечного `down` остаётся Goose CLI, описанный ниже.

Прежде чем писать боту, сгенерируйте код доступа: `go run ./cmd/generate-codes -n 1`
выведет код в консоль и вставит его в `access_codes` как неиспользованный.
После запуска бота напишите ему `/start КОД` — он зарегистрирует студента и
начнёт с Фазы 0 (инструкция).

## Миграции (Goose)

SQL-файлы миграций лежат в `internal/migrations/sql`. Проект уже тянет
`github.com/pressly/goose/v3` как зависимость, поэтому CLI можно вызывать
через `go run` без отдельной установки — версия берётся из `go.mod`,
так что локально и в CI используется один и тот же Goose.

### Локально

```bash
export GOOSE_DRIVER=postgres
export GOOSE_DBSTRING="$DATABASE_URL"   # из .env, например postgres://postgres:postgres@localhost:5432/sa_hr_bot?sslmode=disable

go run github.com/pressly/goose/v3/cmd/goose@v3.27.2 -dir internal/migrations/sql up
```

Другие полезные команды: `... status`, `... down`, `... redo`.

Если предпочитаете постоянно установленный бинарник:

```bash
go install github.com/pressly/goose/v3/cmd/goose@v3.27.2
goose -dir internal/migrations/sql postgres "$DATABASE_URL" up
```

### В контейнере

Продовый образ (`Dockerfile`) содержит только скомпилированный бинарник
бота — сам бот при старте уже накатывает миграции автоматически, отдельный
шаг не нужен. Если всё же требуется накатить миграции вручную из
контейнера (например, перед первым запуском, в CI/CD-пайплайне или чтобы
сделать `down`), удобно временно поднять Go-контейнер с доступом к сети
БД и смонтированным репозиторием:

```bash
docker run --rm \
  -v "$(pwd)":/src -w /src \
  --network host \
  -e GOOSE_DRIVER=postgres \
  -e GOOSE_DBSTRING="$DATABASE_URL" \
  golang:1.25-alpine \
  go run github.com/pressly/goose/v3/cmd/goose@v3.27.2 -dir internal/migrations/sql up
```

(замените `--network host` на имя docker-сети, если PostgreSQL и бот
подняты через `docker network create` / compose — на Linux `--network host`
не нужен macOS/Windows, там достаточно `host.docker.internal` в
`GOOSE_DBSTRING`).

## Локальный запуск в Docker (для теста)

Многоступенчатая сборка (`Dockerfile`): build-стадия на `golang:1.25-alpine`
(версия синхронизирована с `go.mod`, чтобы `go build` не тянул тулчейн из
сети во время сборки образа), финальный образ — `alpine:3.20` с непривилегированным
пользователем и только скомпилированным бинарником + `prompts/` + `media/`. SQL-миграции
никуда копировать не нужно — они встроены в бинарник через `embed.FS` и
применяются автоматически при каждом старте, до начала polling.

```bash
docker build -t sa-hr-bot .
```

Дальше нужен PostgreSQL, доступный из контейнера. Если БД тоже в Docker
(см. шаг 3 выше) — самый простой способ подключить контейнер бота к той же
сети:

```bash
docker run --rm --env-file .env \
  --network container:sa-hr-bot-db \
  sa-hr-bot
```

Либо, если Postgres слушает на хосте (не в Docker), пропишите в `.env`
`DATABASE_URL` с `host.docker.internal` вместо `localhost` (macOS/Windows;
на Linux добавьте `--add-host=host.docker.internal:host-gateway`).

Проверить, что миграции применились и бот стартовал, можно по логам:

```bash
docker logs -f <container_id_или_имя>
```

В логе должна быть строка `goose: successfully migrated database to
version: N` (или `no migrations to run`, если база уже актуальна), затем
`bot starting`.

## Деплой на fly.io

### 1. Инициализация приложения

```bash
flyctl launch
```

`flyctl` найдёт `Dockerfile` и предложит собрать образ по нему. Это
long-polling бот, а не HTTP-сервис — он не слушает никакой порт, поэтому
после `launch` откройте сгенерированный `fly.toml` и уберите/закомментируйте
секцию `[http_service]` (или `[[services]]`, в зависимости от версии
`flyctl`), иначе fly будет ждать ответа на несуществующем порту и считать
машину нездоровой. Также стоит явно выставить `auto_stop_machines = false`,
чтобы машина не останавливалась по признаку "нет входящего HTTP-трафика" —
у бота его никогда и не будет, он сам стучится в Telegram.

### 2. База данных

Нужен доступный по сети PostgreSQL. Два варианта:

- **Postgres на fly.io:**
  ```bash
  fly postgres create --name sa-hr-bot-db
  fly postgres attach sa-hr-bot-db --app <имя-вашего-приложения>
  ```
  `fly postgres attach` сам создаст секрет `DATABASE_URL` в приложении —
  отдельно через `fly secrets set` его прописывать не нужно.

- **Внешняя managed БД** (RDS, Supabase, Neon и т.п.): просто пропишите её
  connection string как секрет `DATABASE_URL` на шаге ниже.

### 3. Секреты

```bash
fly secrets set \
  TELEGRAM_BOT_TOKEN="..." \
  OPENAI_API_KEY="..." \
  ADMIN_IDS="123456789,987654321" \
  SESSION_CYCLE_LIMIT="8" \
  --app <имя-вашего-приложения>
```

`DATABASE_URL` добавлять сюда не нужно, если использовали `fly postgres
attach` (см. выше) — он уже установлен. Если используете внешнюю БД,
добавьте его в эту же команду: `DATABASE_URL="postgres://..."`.
`OPENAI_MODEL`/`OPENAI_BASE_URL` — опциональные секреты, добавляются так же,
по умолчанию не нужны.

### 4. Деплой

```bash
fly deploy
```

Миграции применятся автоматически при старте бота — отдельного шага для
`goose up` в проде не требуется. Логи после деплоя:

```bash
fly logs
```
