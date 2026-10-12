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
go/                       the tier-0/1 port (stdlib only, zero deps)
crates/switchyard-capi/   the C-ABI shim (later, only if we bind)
patches/                  proposed upstream changes (see §5 of the spec)
testdata/corpus/          cases (JSONL) + PIN
testdata/golden/          recorded oracle results (checked in)
testdata/fixtures/        live decider captures, with provenance
docs/                     the harness spec
```

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
corpus + decider fixtures    (checked in, pinned by testdata/corpus/PIN)
      │  verify
      ├────────────► Rust oracle bin  (this crate, links libsy directly)
      └────────────► other impls      (Go port, or the C-ABI shim)
```

The oracle's JSON **defines** the result schema every other implementation must
match.

## Usage

```sh
# run the corpus and record golden results
cargo run -p libsy-conform -- --corpus testdata/corpus --mode record

# re-run and diff against the recorded golden (proves determinism + catches drift)
cargo run -p libsy-conform -- --corpus testdata/corpus --mode verify

# filter for fast iteration
cargo run -p libsy-conform -- --corpus testdata/corpus --mode verify --tier 1 --case 'affinity/*'
```

Exit code is non-zero on any diff, missing/extra case, or error.

## Pinning

The harness is valid against exactly one Switchyard revision, recorded in
`testdata/corpus/PIN` and mirrored in the `rev` of the git dependency in
`Cargo.toml`.

## Licence

Apache-2.0. See `LICENSE` and `NOTICE`.
