// Command foreman is the per-machine agent host: it pairs with a factory,
// reads its inbox for new jobs, spawns one-shot worker processes, and runs
// the worker + brain MCP surfaces.
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
	"strings"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/buildinfo"
	"github.com/pricec/factory/internal/foremanapp"
)

func main() {
	factory := flag.String("factory", "", "factory base URL, e.g. http://factory.example:8080 (required)")
	id := flag.String("id", "", "foreman id, e.g. alpha (required)")
	dataDir := flag.String("data", "./foreman-data", "data directory (inbox.db, auth.db, paired.json)")
	addr := flag.String("addr", ":8090", "HTTP listen address")
	advertise := flag.String("advertise", "", "advertised base URL (default http://<hostname>:<port>)")
	pairingToken := flag.String("pairing-token", os.Getenv("FACTORY_PAIRING_TOKEN"),
		"one-time pairing token (first run only; default $FACTORY_PAIRING_TOKEN)")
	flag.Parse()
	log.Printf("foreman build %s", buildinfo.Current())
	if *factory == "" || *id == "" {
		log.Fatalf("foreman: -factory and -id are required")
	}

	app, err := foremanapp.New(foremanapp.Config{
		FactoryURL:         *factory,
		ID:                 *id,
		DataDir:            *dataDir,
		Addr:               *addr,
		AdvertiseURL:       *advertise,
		PairingToken:       *pairingToken,
		BrainModel:         os.Getenv("FACTORY_FOREMAN_BRAIN_MODEL"),
		WorkerFlashModel:   os.Getenv("FACTORY_WORKER_FLASH_MODEL"),
		WorkerModel:        os.Getenv("FACTORY_WORKER_MODEL"),
		WorkerHardestModel: os.Getenv("FACTORY_WORKER_HARDEST_MODEL"),
		Harness:            strings.ToLower(strings.TrimSpace(os.Getenv("FACTORY_FOREMAN_HARNESS"))),
	})
	if err != nil {
		log.Fatalf("foreman: %v", err)
	}

	if w := claudeLoginWarning([]string{
		os.Getenv("FACTORY_FOREMAN_BRAIN_MODEL"), os.Getenv("FACTORY_WORKER_FLASH_MODEL"),
		os.Getenv("FACTORY_WORKER_MODEL"), os.Getenv("FACTORY_WORKER_HARDEST_MODEL"),
	}, os.Getenv); w != "" {
		log.Print(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("foreman: listen: %v", err)
	}
	srv := &http.Server{Handler: app.Handler()}
	app.MarkListening(ln.Addr().String())
	go app.Run(ctx)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("foreman: serve: %v", err)
		}
	}()
	log.Printf("foreman: %s listening on http://%s (advertised %s)", *id, ln.Addr(), app.AdvertiseURL())
	log.Printf("foreman: paired with %s as %s", *factory, *id)

	// Exit code: 0 on a signal; the self-updater (spec §12) asks for 0
	// after swapping in a new binary and non-zero after a rollback, and
	// systemd (Restart=always) starts whatever bin/foreman now holds.
	code := 0
	select {
	case <-ctx.Done():
	case code = <-app.ExitRequested():
		log.Printf("foreman: auto-update restart requested (exit %d)", code)
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("foreman: shutdown: %v", err)
	}
	if err := app.Close(); err != nil {
		log.Printf("foreman: close: %v", err)
	}
	os.Exit(code)
}
