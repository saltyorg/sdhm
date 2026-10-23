package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/moby/moby/api/types/events"
	mobyclient "github.com/moby/moby/client"
)

func TestEventsBoundsSubscriptionHandshake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		api.eventsFn = func(ctx context.Context, _ mobyclient.EventsListOptions) mobyclient.EventsResult {
			<-ctx.Done()
			errs := make(chan error, 1)
			errs <- ctx.Err()
			return mobyclient.EventsResult{Err: errs}
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		started := time.Now()
		_, errs := newClient(api).Events(ctx, []string{"saltbox"})
		if elapsed := time.Since(started); elapsed != 10*time.Second {
			t.Fatalf("subscription blocked for %v, want 10s", elapsed)
		}
		if err := receive(t, errs); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("subscription error = %v, want deadline exceeded", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("subscription deadline canceled caller: %v", ctx.Err())
		}
	})
}

func TestEventsHandshakeDeadlineDoesNotLimitEstablishedStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		sourceEvents := make(chan events.Message, 1)
		var streamCtx context.Context
		api.eventsFn = func(ctx context.Context, _ mobyclient.EventsListOptions) mobyclient.EventsResult {
			streamCtx = ctx
			return mobyclient.EventsResult{Messages: sourceEvents, Err: make(chan error)}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		client := newClient(api)
		mapped, errs := client.Events(ctx, []string{"saltbox"})
		time.Sleep(time.Minute)
		if streamCtx.Err() != nil {
			t.Fatalf("established stream expired: %v", streamCtx.Err())
		}
		sourceEvents <- events.Message{Action: events.ActionConnect}
		if got := receive(t, mapped); got.Action != "connect" {
			t.Fatalf("event after handshake interval = %+v", got)
		}
		assertNoImmediateValue(t, errs)
		cancel()
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEventsBoundsMobySubscriptionWithoutResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	api, err := mobyclient.New(mobyclient.WithHost(server.URL), mobyclient.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(api)
	client.handshakeTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, errs := client.Events(ctx, []string{"saltbox"})
	if err := receive(t, errs); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled Moby subscription error = %v, want deadline exceeded", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("Moby subscription reached caller deadline: %v", ctx.Err())
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventsMobyStreamSurvivesHandshakeInterval(t *testing.T) {
	releaseEvent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-releaseEvent:
		case <-r.Context().Done():
			return
		}
		if err := json.NewEncoder(w).Encode(events.Message{Action: events.ActionConnect}); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	api, err := mobyclient.New(mobyclient.WithHost(server.URL), mobyclient.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(api)
	client.handshakeTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mapped, errs := client.Events(ctx, []string{"saltbox"})
	timer := time.NewTimer(3 * client.handshakeTimeout)
	defer timer.Stop()
	<-timer.C
	close(releaseEvent)
	if got := receive(t, mapped); got.Action != "connect" {
		t.Fatalf("Moby event after handshake interval = %+v", got)
	}
	assertNoImmediateValue(t, errs)
	cancel()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}
