package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// Server owns the health HTTP listener and serving goroutine.
type Server struct {
	httpServer *http.Server
	done       chan struct{}

	mu       sync.RWMutex
	listener net.Listener
	err      error

	shutdownOnce sync.Once
	shutdownErr  error
}

// NewServer creates a health HTTP server that listens at addr when started.
func NewServer(addr string, handler http.Handler) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
		done: make(chan struct{}),
	}
}

// Start binds the listener synchronously, then starts the serving goroutine.
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	go func() {
		err := s.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	}()

	return nil
}

// Done closes when the serving goroutine exits.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// Err returns the terminal serving error after Done has closed.
func (s *Server) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// Shutdown stops the server once and waits for its serving goroutine to exit.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.httpServer.Shutdown(ctx)
	})

	select {
	case <-s.done:
		return s.shutdownErr
	default:
	}

	select {
	case <-s.done:
		return s.shutdownErr
	case <-ctx.Done():
		select {
		case <-s.done:
			return s.shutdownErr
		default:
		}
		if s.shutdownErr != nil {
			return s.shutdownErr
		}
		return ctx.Err()
	}
}

// Addr returns the bound listener address, or nil before Start succeeds.
func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}
