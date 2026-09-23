package httpx

import (
	"encoding/base64"
	"log/slog"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}
