// Package server runs the HTTP server with graceful shutdown and checks the listen address.
package server

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

// IsLoopbackListen reports whether address listens only on a loopback interface.
func IsLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// InContainer reports whether the process runs in a container.
func InContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	cgroup, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	for _, marker := range []string{"docker", "kubepods", "containerd", "libpod", "lxc"} {
		if bytes.Contains(cgroup, []byte(marker)) {
			return true
		}
	}
	return false
}

// Serve runs the server until ctx ends, then stops new connections and waits up to timeout for open requests.
func Serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.Serve(listener) }()
	select {
	case err := <-errorsChannel:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutdown: waiting up to %s for open requests", timeout)
	shutdownContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("shutdown: %v; closing open connections", err)
		_ = server.Close()
		return err
	}
	log.Print("shutdown: complete")
	return nil
}
