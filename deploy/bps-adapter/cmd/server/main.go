package main

import (
	"context"
	adapter "github.com/jaxson-wang/cpa-plugin-oai-basispoints/internal"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := adapter.ValidateTransportRuntime(); err != nil {
		log.Fatal(err)
	}
	keyPath := os.Getenv("BPS_TRANSPORT_KEY_FILE")
	if keyPath == "" {
		keyPath = "/run/secrets/transport-key"
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatal("cannot read private transport key")
	}
	var config []byte
	if path := os.Getenv("BPS_RELAY_CONFIG_FILE"); path != "" {
		config, err = os.ReadFile(path)
		if err != nil {
			log.Fatal("cannot read relay configuration")
		}
	}
	a, err := adapter.New(string(key), config)
	if err != nil {
		log.Fatal(err)
	}
	a.SetIdentityLogger(log.New(os.Stdout, "", 0))
	addr := os.Getenv("BPS_LISTEN_ADDR")
	if addr == "" {
		addr = ":8000"
	}
	server := &http.Server{Addr: addr, Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 310 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 256 << 10}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		a.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("selected-account BPS adapter listening on %s; CPA %s", addr, adapter.UpstreamCommit)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("private adapter HTTP server failed")
	}
}
