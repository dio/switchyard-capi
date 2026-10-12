package envoycallout

import (
	"context"
	"fmt"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// Handle is the slice of the SDK's HttpFilterHandle an Envoy callout needs.
// Depending on the narrow interface rather than the full handle keeps the adapter
// small and makes it stand-in-able in tests.
type Handle interface {
	HttpCallout(
		cluster string,
		headers [][2]string,
		body []byte,
		timeoutMs uint64,
		cb shared.HttpCalloutCallback,
	) (shared.HttpCalloutInitResult, uint64)
}

// EnvoyCallout implements Callout over the Envoy Go SDK's asynchronous
// HttpCallout, bridged to blocking.
//
// Do blocks, so it MUST run on a goroutine the host scheduled off the worker
// thread — never directly from an OnRequest/OnRequestBody callback. In transit
// that is `w.Go`, which is also where the mutations the decision produces are
// queued for the worker to apply.
type EnvoyCallout struct {
	handle Handle
}

// NewEnvoyCallout adapts an Envoy filter handle.
func NewEnvoyCallout(handle Handle) EnvoyCallout {
	return EnvoyCallout{handle: handle}
}

// Do satisfies Callout.
func (c EnvoyCallout) Do(ctx context.Context, request CalloutRequest) ([]byte, error) {
	waiter := newCalloutWaiter()

	// A non-success init means no callback will fire, so it is reported directly
	// rather than waited on.
	init, _ := c.handle.HttpCallout(
		request.Cluster,
		request.Headers,
		request.Body,
		uint64(request.Timeout.Milliseconds()),
		waiter,
	)
	if init != shared.HttpCalloutInitSuccess {
		return nil, fmt.Errorf("judge callout did not start: %s", initResultName(init))
	}

	return waiter.await(ctx, request.Timeout)
}
