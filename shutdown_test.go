package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeEndpointURL(t *testing.T) {
	for input, expected := range map[string]string{
		"http://192.0.2.1:8000":         "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/":        "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/v1":      "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/v1/":     "http://192.0.2.1:8000",
		"https://example.com/proxy/v1/": "https://example.com/proxy",
		"https://example.com/v10":       "https://example.com/v10",
	} {
		if actual := normalizeEndpointURL(input); actual != expected {
			t.Errorf("normalizeEndpointURL(%q)=%q, want %q", input, actual, expected)
		}
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for address, expected := range map[string]bool{
		"127.0.0.1:1234": true, "localhost:1234": true, "[::1]:1234": true,
		"0.0.0.0:1234": false, ":1234": false, "192.0.2.1:1234": false,
	} {
		if actual := isLoopbackListen(address); actual != expected {
			t.Errorf("isLoopbackListen(%q)=%v", address, actual)
		}
	}
}

func TestServeDrainsOpenStreamOnShutdown(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(writer, "data: %d\n\n", i)
			writer.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, &http.Server{Handler: handler}, listener, 5*time.Second) }()
	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || strings.Count(string(body), "data:") != 5 {
		t.Fatalf("body=%q err=%v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := http.Get("http://" + listener.Addr().String()); err == nil {
		t.Fatal("expected refused connection after shutdown")
	}
}

func TestServeClosesStreamsAfterTimeout(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, &http.Server{Handler: handler}, listener, 200*time.Millisecond) }()
	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after the shutdown timeout")
	}
}
