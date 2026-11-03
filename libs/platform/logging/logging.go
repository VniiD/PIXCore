// Package logging fornece slog JSON com redação de campos sensíveis.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

var sensitive = []string{"password", "secret", "token", "authorization", "key", "dsn", "document", "cpf", "cnpj"}

func redact(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, s := range sensitive {
		if strings.Contains(k, s) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

func New(service, level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv, ReplaceAttr: redact})
	return slog.New(h).With("service", service)
}
