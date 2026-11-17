// Command factory is the controller: it runs the orchestration engine and
// serves the web UI that pairs with foremen and dispatches jobs to them.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/factoryapp"
)

func main() {
	dataDir := flag.String("data", "./factory-data", "data directory for the sqlite stores")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	advertise := flag.String("advertise", "", "advertised base URL (default http://<hostname>:<port>)")
	downloads := flag.String("downloads", "./downloads", "directory holding the foreman binaries served by GET /download (see `make release`)")
	flag.Parse()

	app, err := factoryapp.New(factoryapp.Config{
		DataDir:      *dataDir,
		Addr:         *addr,
		AdvertiseURL: *advertise,
		DownloadsDir: *downloads,
		Budget:       budgets.LoadConfig(),
		Usage:        factoryapp.LoadUsageConfig(),
		Throttle:     factoryapp.LoadThrottleConfig(),
	})
	if err != nil {
		log.Fatalf("factory: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("factory: listen: %v", err)
	}
	srv := &http.Server{Handler: app.Handler()}
	go app.Run(ctx)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("factory: serve: %v", err)
		}
	}()
	log.Printf("factory: listening on http://%s (advertised %s)", ln.Addr(), app.AdvertiseURL())
	log.Printf("factory: planner token: %s", app.PlannerToken())

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("factory: shutdown: %v", err)
	}
	if err := app.Close(); err != nil {
		log.Printf("factory: close: %v", err)
	}
}
