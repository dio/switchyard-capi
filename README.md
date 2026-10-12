# libsy-conform — differential conformance harness for Switchyard `libsy`

`switchyard-capi` hosts the host-side kit for [Switchyard](https://github.com/NVIDIA-NeMo/Switchyard)'s
`libsy` routing core. This mirror of upstream's `switchyard-py` starts with the
piece that is useful **regardless of whether we later bind or port**: a
conformance harness that pins the routing decision, so an independent
implementation can be checked against it and upstream drift becomes measurable.

## Why

`libsy` production code is ~10.9k LOC but the hard part is not the line count. It
is *conformance and drift*: ~10.3k LOC of Rust tests encode the expected
behaviour, and the tuned constants (`MAX_ASSIGNMENTS = 4096`, a 1h session TTL,
the `ClassifyTrigger` variants, `latch_only`) come from benchmark tuning we do
not have the data for. This harness makes "is our implementation still correct,
and what did upstream change?" a mechanical check.

## Layout

```
crates/libsy-conform/     the harness (plain Rust bin → libsy, no FFI)
crates/switchyard-capi/   the C ABI over libsy (pull loop; 2 kinds so far)
go/capi/                  public: cgo wrapper over the Rust C ABI
go/internal/harness/      internal: corpus, result schema, runner plumbing
go/cmd/libsy-conform-impl the runner binary
patches/                  proposed upstream changes (see §5 of the spec)
testdata/fixtures/        live decider captures, with provenance
docs/                     the harness spec, sizing, architecture, libsy's shape
```

> The **corpus and its recorded golden are canonical in
> [`github.com/dio/libsygo`](https://github.com/dio/libsygo)** (its `testdata/`),
> because they are that package's conformance spec and it replays them in its own CI
> with no Rust toolchain and no network. This repo only *reads* them; defaults
> resolve to `../libsygo/testdata`, and `LIBSYGO_TESTDATA` overrides the location.
> Recording truth needs the oracle, the pin and the drift job — which is what lives
> here.

> The **Go implementation of libsy lives in
> [`github.com/dio/libsygo`](https://github.com/dio/libsygo)**, not here. This repo
> is the harness: it pins upstream libsy, records what it does, and diffs
> implementations against that golden. `go/internal/harness` consumes libsygo as
> the `port` backend.

> `go/capi` links the Rust staticlib, so anything that builds the whole `go`
> module needs `cargo build` first — `make go-test` depends on it.

> The tenant **profile contract** (what a profile is, who materializes it, how a
> dataplane resolves one per request) is a dataplane concern and lives with the
> dataplane, not here. Nothing in this repository depends on it.

## Consuming the implementation

The implementation is [`github.com/dio/libsygo`](https://github.com/dio/libsygo):
the Go port of libsy, with its own tests, example, and CI. This repo consumes it
as the `port` backend and diffs it against upstream; it does not own it.

The dependency direction is one-way: **a dataplane imports libsygo, libsygo
imports nothing, and this repo imports libsygo only to test it.** Nothing here
knows about tenants, keys, catalogs, or Envoy.

```go
import (
    "context"

    "github.com/dio/libsygo"
    "github.com/dio/libsygo/jev"
)

// The host supplies the judge call, so egress stays host-side: this type is
// where a dataplane issues its own callout (TLS, credentials, retries, timeout).
type judgeCallout struct{ /* cluster, deadline, ... */ }

func (judgeCallout) Call(ctx context.Context, request jev.DecisionRequest) (jev.DecisionResponse, error) {
    // ... issue the callout through the host's own stack
}

router, err := libsygo.New(profile, models)
outcome := router.Route(ctx, libsygo.Turn{
    Prompt:  prompt,
    Session: tenantID + "/" + sessionID, // must be tenant-namespaced
}, judgeCallout{})
// outcome.SelectedModelIDs is the fallback order, chosen model first.
// outcome.Evidence explains the decision.
```

`libsygo` performs **no I/O** — it never resolves a URL, holds a credential, or
opens a socket. `jev` is a convenience client for hosts that have no HTTP stack of
their own (and for this harness); a dataplane with its own egress implements
`libsygo.Decider` directly and uses `jev` only for the IR types.

Cache the built `Router` by `(tenant, profile ID, revision)`, and namespace
session ids per tenant: two tenants sharing a session id would otherwise share
retained routing state.

## Pin policy

The pin is a **commit, not a release tag**. The tier-0/1 JEV path
(`Step::CallDecision`, `CapabilityJudgeConfig::Decision`, `DecisionResponse`) does
**not** exist in the released `v0.3.0` — it landed after that tag. Verified by
evaluating `v0.3.0` through the drift job, which fails to compile against this
harness (`unresolved import switchyard_libsy::DecisionJudgeConfig`, no
`Step::CallDecision` variant, no `DecisionResponse`).

So tracking a release would silently drop the entire decision-judge path. The
scheduled drift job keeps that visible and distinguishes the two failure modes:

- **revision cannot be evaluated** — upstream changed API shape; update the harness
- **behavioural drift** — same API, different decisions; the diff is the answer

## Tier-0/1 port

`go/` ports the decision core that owns nothing but the judge plumbing: the
JEV / System One decision judge, the cutoff, fail-open, the session latch, and
candidate ordering. It is **510 LOC of stdlib Go with zero dependencies** (~220 of
which are the port proper; the rest is corpus/result plumbing), and it is diffed
against the oracle on every run:

```sh
make impl     # cargo run -p libsy-conform -- --mode impl --tier 1
```

Tier 3 (`stage_router` / tool signals) is deliberately **not** ported — that is
the module where porting is most expensive, most churn-prone, and least
verifiable, so the recommendation is to bind it. The port rejects anything
outside tier 0/1 with a clear error rather than approximating it.

## Chain of trust

```
libsy Rust unit tests        (upstream, authoritative)
      │  record
      ▼
corpus + decider fixtures    (canonical in libsygo; pinned by its testdata/corpus/PIN)
      │  verify
      ├────────────► Rust oracle bin  (this crate, links libsy directly)
      └────────────► other impls      (Go port, or the C-ABI shim)
```

The oracle's JSON **defines** the result schema every other implementation must
match.

## Usage

```sh
# run the corpus and record golden results (into libsygo's testdata/)
cargo run -p libsy-conform -- --mode record

# re-run and diff against the recorded golden (proves determinism + catches drift)
cargo run -p libsy-conform -- --mode verify

# filter for fast iteration
cargo run -p libsy-conform -- --mode verify --tier 1 --case 'affinity/*'
```

Exit code is non-zero on any diff, missing/extra case, or error.

## Pinning

The harness is valid against exactly one Switchyard revision, recorded in libsygo's
`testdata/corpus/PIN` and mirrored in the `rev` of the git dependency in
`Cargo.toml`.

## Licence

Apache-2.0. See `LICENSE` and `NOTICE`.
