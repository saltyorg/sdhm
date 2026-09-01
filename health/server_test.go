package health

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServerStartBindsSynchronously(t *testing.T) {
	server := NewServer("127.0.0.1:0", http.NotFoundHandler())
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})

	addr := server.Addr()
	if addr == nil {
		t.Fatal("Addr() = nil after successful Start()")
	}

	conn, err := net.Dial(addr.Network(), addr.String())
	if err != nil {
		t.Fatalf("Dial(%q) error = %v", addr, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close test connection: %v", err)
	}
}

func TestServerStartReturnsOccupiedAddressError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen occupied address: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	server := NewServer(occupied.Addr().String(), http.NotFoundHandler())
	if err := server.Start(); err == nil {
		t.Fatal("Start() error = nil, want occupied address error")
	}
}

func TestServerShutdownClosesDoneAndReturnsNil(t *testing.T) {
	server := startTestServer(t)

	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	assertDone(t, server.Done())
	if err := server.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after shutdown", err)
	}
}

func TestServerUnexpectedListenerCloseSetsErr(t *testing.T) {
	server := startTestServer(t)

	if err := server.listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	assertDone(t, server.Done())
	if err := server.Err(); err == nil {
		t.Fatal("Err() = nil, want unexpected listener error")
	}
}

func TestServerShutdownIsIdempotent(t *testing.T) {
	server := startTestServer(t)

	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	assertDone(t, server.Done())
}

func TestServerTimeouts(t *testing.T) {
	server := NewServer("127.0.0.1:0", http.NotFoundHandler())

	if got, want := server.httpServer.ReadHeaderTimeout, 5*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout = %s, want %s", got, want)
	}
	if got, want := server.httpServer.ReadTimeout, 10*time.Second; got != want {
		t.Errorf("ReadTimeout = %s, want %s", got, want)
	}
	if got, want := server.httpServer.WriteTimeout, 10*time.Second; got != want {
		t.Errorf("WriteTimeout = %s, want %s", got, want)
	}
	if got, want := server.httpServer.IdleTimeout, 60*time.Second; got != want {
		t.Errorf("IdleTimeout = %s, want %s", got, want)
	}
}

func startTestServer(t *testing.T) *Server {
	t.Helper()

	server := NewServer("127.0.0.1:0", http.NotFoundHandler())
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})
	return server
}

func assertDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Done() did not close")
	}
}
