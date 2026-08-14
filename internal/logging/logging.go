package logging

import (
	"log/slog"
	"os"
)

func Setup(format string) {
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, nil)
	} else {
		h = slog.NewJSONHandler(os.Stdout, nil)
	}

	slog.SetDefault(slog.New(h))
}
