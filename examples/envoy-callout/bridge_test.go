package envoycallout

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// buffer builds an UnsafeEnvoyBuffer over a Go slice. The SDK's fields are
// exported, so a callback can be driven directly — no Envoy required.
func buffer(body []byte) shared.UnsafeEnvoyBuffer {
	if len(body) == 0 {
		return shared.UnsafeEnvoyBuffer{}
	}
	return shared.UnsafeEnvoyBuffer{Ptr: &body[0], Len: uint64(len(body))}
}

func TestOnHttpCalloutDoneJoinsAndCopiesBody(t *testing.T) {
	waiter := newCalloutWaiter()
	first := []byte(`{"answers":`)
	second := []byte(`{}}`)
	waiter.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil,
		[]shared.UnsafeEnvoyBuffer{buffer(first), buffer(second)})

	body, err := waiter.await(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	const want = `{"answers":{}}`
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}

	// The buffers are only valid during the callback, so the body must be a copy.
	first[0] = 'X'
	if string(body) != want {
		t.Fatalf("body aliased the callback buffer: %q", body)
	}
}

func TestAwaitReportsNonSuccessResult(t *testing.T) {
	waiter := newCalloutWaiter()
	waiter.OnHttpCalloutDone(1, shared.HttpCalloutReset, nil, nil)

	_, err := waiter.await(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("err = %v, want a reset failure", err)
	}
}

func TestAwaitStopsOnTimeout(t *testing.T) {
	waiter := newCalloutWaiter()

	_, err := waiter.await(context.Background(), 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// A reply landing after the deadline must not block the thread delivering it:
// that thread is the Envoy worker, and stalling it stalls the stream.
func TestLateCallbackDoesNotBlock(t *testing.T) {
	waiter := newCalloutWaiter()
	if _, err := waiter.await(context.Background(), 10*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}

	delivered := make(chan struct{})
	go func() {
		waiter.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil,
			[]shared.UnsafeEnvoyBuffer{buffer([]byte("late"))})
		close(delivered)
	}()

	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("OnHttpCalloutDone blocked after the deadline passed")
	}
}

func TestAwaitHonoursContextCancellation(t *testing.T) {
	waiter := newCalloutWaiter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := waiter.await(ctx, time.Second); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
