# SA HR Bot

Telegram-бот для тренировки технических собеседований системных аналитиков.
Пользователь регистрируется по одноразовому коду, проходит квалификацию, урок
КДИР и тематические блоки с обратной связью от LLM.

## Запуск локально

### Что понадобится

- Go 1.25+;
- Docker с Docker Compose;
- токен Telegram-бота;
- ключ OpenAI API.

### 1. Настроить окружение

```bash
git clone https://github.com/CodeHunt7/sa-hr-bot.git
cd sa-hr-bot
cp .env.example .env
```

Заполнить в `.env`:

```dotenv
TELEGRAM_BOT_TOKEN=...
OPENAI_API_KEY=...
DATABASE_URL=postgres://postgres:localpass@localhost:5432/sahrbot?sslmode=disable
```

Опционально:

```dotenv
ADMIN_IDS=123456789
SESSION_CYCLE_LIMIT=8
```

### 2. Поднять PostgreSQL

```bash
make docker-db-up
```

### 3. Запустить бота

```bash
make run
```

При старте бот сам применяет миграции и загружает активный банк из
`materials/question_bank.csv`. В логах должны появиться:

```text
question bank synchronized
bot starting
```

### 4. Создать код и начать тест

В другом окне терминала:

```bash
make code
```

Отправить полученный код боту:

```text
/start SA2026-XXXX
```

Остановить бота — `Ctrl+C`. Остановить локальную БД:

```bash
make docker-db-down
```

Полезные команды:

```bash
make check       # тесты, vet и сборка
make questions   # вручную синхронизировать банк вопросов
make code        # создать один код доступа
```

Подробный сценарий ручной проверки находится в
[`materials/how-to-test.md`](materials/how-to-test.md).

## Запуск на VPS

Для текущей production-схемы нужны:

- Linux-сервер с Git, Docker и Docker Compose;
- каталог `/opt/sa-hr-bot`;
- серверные файлы `compose.prod.yaml` и `prod.env`.

`prod.env` должен содержать как минимум:

```dotenv
TELEGRAM_BOT_TOKEN=...
OPENAI_API_KEY=...
DATABASE_URL=postgres://...
ADMIN_IDS=123456789
SESSION_CYCLE_LIMIT=8
```

Секреты, настоящий IP сервера, `prod.env` и пароли нельзя сохранять в Git.

### Первый запуск

```bash
ssh root@IP_СЕРВЕРА
cd /opt
git clone https://github.com/CodeHunt7/sa-hr-bot.git
cd sa-hr-bot

# Добавить на сервер compose.prod.yaml и prod.env
docker compose -f compose.prod.yaml up -d --build bot
docker compose -f compose.prod.yaml logs --tail=100 bot
```

Запуск успешен, если в логах есть `question bank synchronized`, затем
`bot starting`, а контейнер не завершается с ошибкой.

### Обновить бота после изменений в `main`

```bash
ssh root@IP_СЕРВЕРА
cd /opt/sa-hr-bot

git pull --ff-only origin main
docker compose -f compose.prod.yaml up -d --build bot
docker compose -f compose.prod.yaml logs --tail=100 bot
```

Миграции и новый банк вопросов применятся автоматически при старте.

Если Docker явно использует некорректный кэш:

```bash
docker compose -f compose.prod.yaml build --no-cache bot
docker compose -f compose.prod.yaml up -d --force-recreate bot
```

### Состояние и логи

```bash
cd /opt/sa-hr-bot

docker compose -f compose.prod.yaml ps
docker compose -f compose.prod.yaml logs --tail=100 bot
docker compose -f compose.prod.yaml logs -f bot
```

`Ctrl+C` останавливает только просмотр логов, сам бот продолжает работать.

## Администрирование

Администраторы задаются Telegram ID через `ADMIN_IDS`:

```dotenv
ADMIN_IDS=123456789,987654321
```

После изменения `prod.env` пересоздать контейнер:

```bash
docker compose -f compose.prod.yaml up -d --force-recreate bot
```

Команда `/admin` показывает справку. Команды доступны только администраторам:

- `/code` или `/code 5` — создать от 1 до 20 одноразовых кодов;
- `/codes` — показать свободные и занятые коды;
- `/delete_code SA2026-XXXX` — удалить неиспользованный код;
- `/tokens SA2026-XXXX` — посмотреть расход токенов пользователя;
- `/report` — общий отчёт по пользователям, кодам и токенам.

Использованный код удалить нельзя: он связан с пользователем и историей
тренировок. Отзыва доступа зарегистрированного пользователя пока нет.

## Стек

- Go и `telebot.v3`;
- PostgreSQL и `pgx`;
- Goose для миграций;
- OpenAI Go SDK;
- Docker и Docker Compose.

## Структура

```text
cmd/          точки входа бота и служебных утилит
internal/     бизнес-логика, БД, LLM и Telegram-обработчики
prompts/      системный промпт LLM
media/        изображения и видео урока
materials/    документация и банки вопросов
```

Активный банк: `materials/question_bank.csv` — 80 вопросов. Архивные банки
имеют даты в названиях. Целевая логика работы описана в
[`materials/workflow-new.md`](materials/workflow-new.md).
