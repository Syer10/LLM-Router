// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
// It defaults to "dev" for local builds.
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("llm-router " + version)
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	r := NewRouter(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start owned processes, then discover models from every backend.
	r.StartOwned(ctx)
	r.DiscoverAll(ctx)
	r.StartLoops(ctx)

	mux := http.NewServeMux()
	// Serve the whole OpenAI surface with or without the /v1 prefix, so
	// clients work whether their base URL is ".../v1" or just the host root.
	// The "/" catch-all routes every non-admin, non-health path to HandleV1,
	// which transparently proxies it to a backend.
	mux.HandleFunc("/v1", r.HandleV1)
	mux.HandleFunc("/v1/", r.HandleV1)
	mux.HandleFunc("/", r.HandleV1)
	mux.HandleFunc("/admin/", r.HandleAdmin)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("llm-router listening on %s (%d backends)", cfg.Listen, len(r.order))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	r.StopAll()
}
