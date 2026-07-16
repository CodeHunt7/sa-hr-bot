package main

import (
	"strings"
	"testing"
)

func TestRedactConfiguredSecrets(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456789:telegram-secret")
	t.Setenv("OPENAI_API_KEY", "sk-openai-secret")
	t.Setenv("DATABASE_URL", "postgres://user:password@localhost/db")

	got := redactConfiguredSecrets(
		"telegram https://api.telegram.org/bot123456789:telegram-secret/getMe " +
			"openai sk-openai-secret db postgres://user:password@localhost/db",
	)

	for _, secret := range []string{"telegram-secret", "sk-openai-secret", "password@localhost"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q was not redacted: %s", secret, got)
		}
	}
	if strings.Count(got, "[REDACTED]") != 3 {
		t.Fatalf("expected all three configured secrets to be redacted, got: %s", got)
	}
}
