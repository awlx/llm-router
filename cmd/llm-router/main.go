// Command llm-router routes OpenAI-compatible requests to the endpoint that serves the requested model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/awlx/llm-router/internal/config"
	"github.com/awlx/llm-router/internal/router"
	"github.com/awlx/llm-router/internal/server"
	"github.com/awlx/llm-router/internal/version"
)

func main() {
	configPath := flag.String("config", "", "path to the JSON config file (default: $LLM_ROUTER_CONFIG)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	path := *configPath
	if path == "" {
		path = os.Getenv("LLM_ROUTER_CONFIG")
	}
	if path == "" {
		log.Fatal("set -config or LLM_ROUTER_CONFIG")
	}
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	llmRouter, err := router.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if server.IsLoopbackListen(cfg.Listen) && server.InContainer() {
		log.Printf("warning: listen address %s is loopback inside a container; clients outside the container cannot connect. Use 0.0.0.0:<port> to publish the port", cfg.Listen)
	}
	llmRouter.Discover()
	go llmRouter.RunDiscovery()
	httpServer := &http.Server{
		Handler:           llmRouter,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("llm-router %s listening on %s with %d endpoints", version.Version, cfg.Listen, len(cfg.Endpoints))
	if err := server.Serve(ctx, httpServer, listener, time.Duration(cfg.ShutdownTimeoutSeconds)*time.Second); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
