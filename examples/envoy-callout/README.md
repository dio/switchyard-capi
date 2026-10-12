# envoy-callout

Shows how a host serves `decision.Decider` over an Envoy HTTP callout.

The routing library performs no I/O: the judge call is the host's to make. That is
the seam this example fills, and it has three parts.

| Part | Role |
|---|---|
| `Callout` | A blocking HTTP callout. The host implements it over whatever egress it owns. |
| `Decider` | The glue: encode the neutral request, call out, decode the reply back onto the neutral response. |
| `EnvoyCallout` | One such implementation, over the upstream Envoy Go SDK's callback-style `HttpCallout`, bridged to blocking. |

## Why a separate module

The library module has **no dependencies**. This example needs the Envoy Go SDK,
so it lives in its own module and the library stays free of it:

```
go/                       github.com/dio/switchyard-capi/go           (stdlib only)
examples/envoy-callout/   github.com/dio/switchyard-capi/examples/... (+ Envoy SDK)
```

Nothing here imports a dataplane. A dataplane imports the library.

## The bridge is the interesting part

Envoy's `HttpCallout` is asynchronous: the reply arrives at a callback on the
thread processing the stream. The router wants a blocking call. Bridging the two
is where the subtleties live, so that is what the tests cover:

- **The callback must not block.** It is running on the Envoy worker; stalling it
  stalls the stream. The reply goes into a buffered channel (`calloutWaiter`), so
  a reply that arrives *after* the deadline is dropped rather than blocking.
- **Buffers are borrowed.** `UnsafeEnvoyBuffer` is only valid for the duration of
  the callback, so the body is copied and Envoy's possible chunking is joined.
- **Blocking happens off-worker.** `EnvoyCallout.Do` blocks, so it must run on a
  goroutine the host scheduled off the worker thread, never from an `OnRequest`
  callback.

## Using it

```go
judge := envoycallout.NewDecider(
    envoycallout.NewEnvoyCallout(handle), // shared.HttpFilterHandle
    "judge-cluster",                      // Envoy cluster for the judge endpoint
    "/v1/systemone",                      // judge endpoint path
    "api.typesafe.ai",                    // :authority
    5*time.Second,
)

router, err := decision.New(profile, models, judge)
outcome := router.Route(ctx, decision.Turn{Prompt: prompt, Session: tenant + "/" + session})
```

`judge` satisfies `decision.Decider`, so it drops straight into `decision.New`.

## Plugging a different stack in

Transit's `up.Writer` already has the same callout, so its adapter is a
translation — and it must run inside `w.Go`, because `Writer.Do` panics outside it:

```go
func (c transitCallout) Do(ctx context.Context, request CalloutRequest) ([]byte, error) {
    response, err := c.w.Do(ctx, up.HTTPCalloutRequest{
        Cluster:       request.Cluster,
        Headers:       request.Headers,
        Body:          request.Body,
        TimeoutMillis: uint64(request.Timeout.Milliseconds()),
    })
    if err != nil {
        return nil, err
    }
    if response.Result != up.HTTPCalloutSuccess {
        return nil, fmt.Errorf("judge callout failed: %d", response.Result)
    }
    var body []byte
    for _, chunk := range response.Body {
        body = append(body, chunk.ToBytes()...)
    }
    return body, nil
}
```

## Tests

```sh
cd examples/envoy-callout && go test ./...
```

Fourteen tests, ~84% coverage. They need **no Envoy**: `UnsafeEnvoyBuffer`'s
fields are exported, so `OnHttpCalloutDone` can be driven directly, and the SDK
handle is behind a narrow `Handle` interface.

| Area | Covered |
|---|---|
| Bridge | joins and copies chunked bodies; reports a non-success result; times out; a late reply does not block; respects context cancellation |
| Decider | encodes and decodes a real captured verdict; sends the host-owned cluster/path/authority/timeout; surfaces encode, callout, and decode failures |
| Envoy adapter | translates the request; reports an init failure; propagates a callout failure |
| End to end | a host callout alone drives `decision.Router`: advantage `0.77 > cutoff 0.4` → `smart`, with fallback order and evidence |

What needs Envoy is the *runtime*, not the logic: an actual `.so` loaded by Envoy
exercising a real callout is e2e, and belongs with the dataplane that wires it.
