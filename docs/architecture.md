# Architecture

Text form of the upstream "Core components" diagram
(`Switchyard/docs/assets/switchyard-components.png`), for this stack. Same idea —
who calls whom, and what is shared — with the boundary that matters here made
explicit: **the library performs no I/O; the host owns egress.**

## Overview

> The host module in the diagram is `examples/llm-decider` in **dio/transit**. It
> *consumes* this repository; nothing here imports it. The diagram is here because
> this repo documents how a host uses the library, the same way
> `examples/envoy-callout` does.

```
                       ┌───────────────────────────┐
                       │     Agent / API caller    │
                       └─────────────┬─────────────┘
                                     │ HTTP
                       ┌─────────────▼─────────────┐
                       │   Envoy dataplane (EG)    │  owns egress: TLS,
                       │   listener · HCM · route   │  credentials, retries,
                       └─────────────┬─────────────┘  timeouts
                                     │
   ┌─────────────────────────────────┼─────────────────────────────────┐
   │        llm-decider  (one Go module, one .so, five entry points)    │
   │                                                                    │
   │  llm-decider           downstream filter    ── decides ──          │
   │  llm-decider-upstream  upstream filter      ── shapes  ──          │
   │  llm-decider-cluster   cluster extension    ── selects ──          │
   │  llm-decider-lb        LB policy            ── chooses ──          │
   │  llm-decider-accesslog access logger        ── records ──          │
   └─────────────────────────────────┬─────────────────────────────────┘
                                     │ imports
                       ┌─────────────▼─────────────┐
                       │   go/decision (library)   │  routing core.
                       │   cascade · latch         │  NO I/O, no sockets,
                       │   cutoff · fail-open      │  no credentials
                       │   candidate ordering      │
                       └─────────────┬─────────────┘
                                     │ judge call, host-supplied (Decider)
                       ┌─────────────▼─────────────┐
                       │        go/jev             │  System One IR.
                       │   encode / decode, client │  Optional HTTP client
                       └─────────────┬─────────────┘  for hosts without one
                                     │ HTTP (the host's callout)
                       ┌─────────────▼─────────────┐
                       │   Judge endpoint (JEV)    │  typed probabilistic
                       └───────────────────────────┘  decisions, no strings
```

## The boundary

One rule explains the whole layout:

```
  host (Envoy, transit, anything)      library (go/decision)
  ───────────────────────────────      ─────────────────────
  sockets, TLS, credentials            cascade, cutoff, latch
  retries, timeouts, DNS               fail-open, ordering, evidence
  the judge HTTP call                  the question and the verdict
```

`decision.Decider` is the seam. The host implements one method and the library
never learns what transport it used:

```go
type Decider interface {
    Call(ctx context.Context, request jev.DecisionRequest) (jev.DecisionResponse, error)
}
```

That is why the examples in this repo can be tested with no Envoy at all: a host
callout is just a value.

## Request path

```
 1.  caller ──POST /{"prompt": …}──▶ Envoy listener
 2.  downstream filter          session key → per-stream slot
 3.    └─ body callback         prompt → decision.Turn
 4.       └─ w.Go (off-worker)  ──▶ decision.Router.Route(turn, decider)
 5.            └─ decider        ──HTTP──▶ judge        (host's egress)
 6.               ◀── verdict    p(advantage) = 0.77
 7.            cutoff 0.4       capable > efficient  →  "smart"
 8.       ◀── outcome           x-llm-decided-model: smart
 9.  cluster extension          host whose model == smart
10.  upstream filter            shape for "smart"
11.  ──▶ upstream ──▶ caller
12.  access logger              decided_model="smart" source="decision_classifier"
```

Steps 4–5 are why the decision is asynchronous: a judge round trip is 70–500 ms,
so it must never occupy the Envoy worker.

## Where the network is

| Boundary | Caller | Offline alternative |
|---|---|---|
| judge call | host (Envoy cluster, transit `Writer.Do`) | — |
| judge endpoint | `go/jev` client, for hosts with no HTTP stack | — |
| **routing core** | nobody | always local |
| **`stage_router` (tier 3)** | **nobody** | **fully local, no network** |

A profile whose algorithm makes no judge call is expressible, and emits
`steps: ["Done"]` with zero host calls — verified against the oracle, and the
reason `tool_signals` is documented as pure and I/O-free.

## Conformance

```
  upstream libsy (Rust)          pinned revision, testdata/corpus/PIN
        │  record
        ▼
  corpus + decider fixtures      checked in
        │  verify                  ├──▶ Rust oracle bin  (links libsy directly)
        ▼                          └──▶ Go implementation (go/decision)
     diff: tier · ordered candidates · evidence · rewritten request
```

The oracle's JSON defines the result schema; the Go implementation is diffed
against it turn for turn. `scripts/drift-check.sh` re-pins to a newer upstream
revision and reports the behavioural diff, which is how drift is measured rather
than assumed.
