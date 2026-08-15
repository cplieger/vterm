// Command e2etestserver backs the typed-framing e2e test: a real terminal
// Handler (running /bin/cat) plus a minimal harness page, listening on an
// ephemeral 127.0.0.1 port printed to stdout. The Playwright test spawns it
// (`go run ./internal/e2etestserver` from the repo root), drives the REAL
// browser client against it, and kills it when done; the stdin watchdog
// exits the server if the test process dies without cleanup.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cplieger/web-terminal-engine/v4/terminal"
)

const harnessPage = `<!doctype html><html><head><meta charset="utf-8"></head>
<body><div id="out"></div></body></html>`

func main() {
	h := terminal.NewHandler([]string{"/bin/cat"}, terminal.WithLogger(nil))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, harnessPage)
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// The e2e test parses this exact line to find the server.
	fmt.Printf("LISTEN http://%s\n", ln.Addr().String())

	// Stdin watchdog: the test holds our stdin pipe open; when the test
	// process exits (even killed), stdin closes and we shut down instead of
	// leaking a /bin/cat PTY on the machine.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
