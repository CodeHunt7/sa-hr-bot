# Builder pinned to match go.mod's `go 1.25.7` directive exactly (bumped
# there by the goose dependency). Keeping the tag in sync avoids `go
# build` silently downloading a newer toolchain mid-build (GOTOOLCHAIN=auto).
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/bot ./cmd/bot

# Minimal runtime image. Goose migrations don't need to be copied here:
# internal/migrations embeds internal/migrations/sql/*.sql into the
# binary at build time (go:embed), and main.go applies them against
# DATABASE_URL on every startup, before the bot begins polling.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S app -G app

COPY --from=build /out/bot /usr/local/bin/bot
COPY prompts /app/prompts

WORKDIR /app
USER app

ENTRYPOINT ["bot"]
