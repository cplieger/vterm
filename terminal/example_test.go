package terminal_test

import (
	"log/slog"
	"net/http"

	"github.com/cplieger/web-terminal-engine/v4/terminal"
)

func Example() {
	h := terminal.NewHandler(
		[]string{"/bin/bash"},
		terminal.WithWorkDir("/home/user"),
		terminal.WithLogger(slog.Default()),
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	_ = mux
	// Output:
}
