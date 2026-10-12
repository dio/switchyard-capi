// Package envoycallout shows how a host serves `libsygo.Decider` over an Envoy
// HTTP callout.
//
// The library performs no I/O, so the judge call is the host's to make. That is
// the seam this package fills, and it has three parts:
//
//   - [Callout] — a blocking HTTP callout. The host implements it over whatever
//     egress it owns (Envoy, or anything else).
//   - [Decider] — the glue: encode the neutral request, call out, decode the
//     reply back onto the neutral response.
//   - [EnvoyCallout] — one such implementation, over the upstream Envoy Go SDK's
//     callback-style `HttpCallout`, bridged to blocking.
//
// The example lives in its own module so the library stays dependency-free: this
// module needs the Envoy SDK, the library does not.
package envoycallout

import (
	"context"
	"fmt"
	"time"

	"github.com/dio/libsygo/jev"
)

// CalloutRequest is one callout the host must perform. The field names mirror the
// Envoy SDK's HttpCallout, so an adapter is a direct translation.
type CalloutRequest struct {
	// Cluster is the upstream to call. For a judge this is a host-configured
	// endpoint, not a tenant-supplied value.
	Cluster string

	// Headers must include the pseudo-headers Envoy needs (":method", ":path",
	// ":scheme", and either "host" or ":authority").
	Headers [][2]string

	// Body is the encoded judge request.
	Body []byte

	// Timeout bounds the call. It is part of the request's latency budget, so
	// the host should surface it rather than inventing one.
	Timeout time.Duration
}

// Callout performs one blocking HTTP callout.
//
// Implementations MUST be safe to call from a goroutine that is not the Envoy
// worker thread, because the decision runs off-worker so it never blocks the
// worker on a judge round trip (70-500 ms for System One).
type Callout interface {
	Do(ctx context.Context, request CalloutRequest) ([]byte, error)
}

// Decider adapts a blocking Callout into the judge call that libsygo.Router
// needs, so it can be passed straight to libsygo.New.
type Decider struct {
	callout   Callout
	cluster   string
	path      string
	authority string
	timeout   time.Duration
}

// NewDecider builds a judge Decider over a host callout.
//
// path is the judge endpoint (for System One, "/v1/systemone") and authority the
// host header to present. Both are host configuration, not tenant input.
func NewDecider(callout Callout, cluster, path, authority string, timeout time.Duration) *Decider {
	return &Decider{
		callout:   callout,
		cluster:   cluster,
		path:      path,
		authority: authority,
		timeout:   timeout,
	}
}

// Call satisfies libsygo.Decider.
func (d *Decider) Call(ctx context.Context, request jev.DecisionRequest) (jev.DecisionResponse, error) {
	body, err := jev.EncodeRequest(request)
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("encoding judge request: %w", err)
	}

	reply, err := d.callout.Do(ctx, CalloutRequest{
		Cluster: d.cluster,
		Headers: [][2]string{
			{":method", "POST"},
			{":path", d.path},
			{":scheme", "https"},
			{":authority", d.authority},
			{"content-type", "application/json"},
		},
		Body:    body,
		Timeout: d.timeout,
	})
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("judge callout: %w", err)
	}

	// Decoding validates the reply against the request that produced it, so the
	// original request has to be carried through — the reply alone cannot be
	// checked.
	response, err := jev.DecodeResponse(reply, request)
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("decoding judge response: %w", err)
	}
	return response, nil
}
