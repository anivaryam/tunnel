package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anivaryam/tunnel/internal/server"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	publicHost := os.Getenv("PUBLIC_HOST")
	baseDomain := os.Getenv("BASE_DOMAIN")

	cfWorkerEnabled := os.Getenv("CF_WORKER_ENABLED") == "true"
	var wsOrigins []string
	if v := os.Getenv("TUNNEL_WS_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				wsOrigins = append(wsOrigins, o)
			}
		}
	}
	cfg := server.Config{
		Port:             port,
		PublicHost:       publicHost,
		BaseDomain:       baseDomain,
		WorkerSecret:     os.Getenv("TUNNEL_WORKER_SECRET"),
		CFWorkerEnabled:  cfWorkerEnabled,
		SingleTunnelMode: os.Getenv("SINGLE_TUNNEL_MODE") == "true",
		WSOriginPatterns: wsOrigins,
	}
	s := server.New(cfg)
	defer s.Close()
	log.Printf("tunnel-server version=%s", version)

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           s.Handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("relay server listening on :%s", port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	select {
	case <-quit:
	case err := <-serverErr:
		log.Printf("server error: %v", err)
		return 1
	}

	log.Println("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := httpSrv.Shutdown(ctx); err != nil {
		cancel()
		log.Printf("shutdown error: %v", err)
		return 1
	}
	cancel()
	log.Println("server stopped")
	return 0
}
