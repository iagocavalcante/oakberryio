package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPCheckerHTTPPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	c := HTTPChecker{}
	if err := c.Healthy(context.Background(), host, port, "/health"); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	if err := c.Healthy(context.Background(), host, port, "/missing"); err == nil {
		t.Fatal("want error for non-2xx path")
	}
}

func TestHTTPCheckerTCPPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	c := HTTPChecker{}
	if err := c.Healthy(context.Background(), host, port, ""); err != nil {
		t.Fatalf("healthy: %v", err)
	}
}

func TestHTTPCheckerNothingListeningFails(t *testing.T) {
	// Reserve a port, then close it so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c := HTTPChecker{}
	if err := c.Healthy(ctx, host, port, ""); err == nil {
		t.Fatal("want error when nothing is listening")
	}
}
