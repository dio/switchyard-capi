# libsy Differential Harness

**Status:** Spec. Scope: a conformance harness that runs a request corpus through
both the pinned Rust `libsy` core (oracle) and a Go implementation (port or, if
we bind, the C-ABI shim), then diffs the routing decisions field by field.

**Why this exists.** The cost of owning routing in Go is not the line count —
`libsy` production code is ~10.9k LOC. The cost is *conformance and drift*: the
10.3k LOC of Rust tests are the spec, and the tuned constants (`MAX_ASSIGNMENTS =
4096`, 1h `SESSION_STATE_TTL`, `ClassifyTrigger` variants, `latch_only`) encode
benchmark tuning we do not have the data for. This harness turns "did we port it
right, and is it still right after upstream moves?" into a mechanical check.

**Chain of trust:**

```
libsy Rust unit tests           (upstream, authoritative behaviour)
        │  record
        ▼
corpus + decider fixtures       (checked in, versioned with the pinned commit)
        │  verify
        ├──────────────► Rust oracle bin   (plain bin → libsy, no FFI) ─┐
        └──────────────► Go runner         (port)                        ─┴─► diff
```

The oracle is a **plain Rust binary that links `libsy` directly** — no FFI, no
shim. That keeps the first cut cheap: the corpus, the fixtures, and the diff are
the deliverable, and they are worth having before any binding decision is made.
The oracle's serialisation is what *defines* the result schema the Go side must
match.

If we later bind instead of port, the same bin is re-pointed at the C-ABI shim
(§5) and a small set of ABI-fidelity cases is cross-checked against the Python
binding (`switchyard-py`) to prove the shim adds no behaviour. Until then there
is no shim to validate.

---

## 1. Determinism audit

The harness is only useful if a single case is reproducible. libsy is mostly
pure; the exceptions are enumerated here and must be controlled.

| Source | Where | Control |
|---|---|---|
| `tool_signals.rs` | "All logic is pure and deterministic — no I/O, no shared state." | none needed |
| `State` | `turn_count`, `tool_signals`, `extra: HashMap<String, StateValue>` | inspectable; reset per case |
| Session table | `FallThrough` per-session `State`, keyed by `RoutingIdentity` | reset per case; no TTL expiry mid-corpus |
| Affinity map | `AffinityRouter.assignments`, bounded `MAX_ASSIGNMENTS = 4096` | reset per case; assert never at bound |
| RNG | `rand.rs`: `seed: Option<u64>`; `None => rand::make_rng()` | **seed is mandatory** for `random` profiles |
| `outcome_id` | `OutcomeMetadata` UUIDv7 (time-based) | **masked** in the diff |
| Timing | `Instant::now()` in `observability.rs`, `core/algorithm.rs`, `advisor_gate.rs`, `fall_through.rs` | metrics/duration only; **masked** in the diff |
| Session eviction | `SESSION_STATE_TTL = 1h`, background cleanup task | keep corpus cases short-lived; never rely on eviction |

Rules: **seed every stochastic profile**, **reset the session/affinity tables
between cases** unless the case declares a session sequence (§4), and **mask
`outcome_id` and all timings** in the comparator.

---

## 2. Corpus format

One JSON object per line (JSONL), UTF-8, `\n` terminated. Blank lines and lines
starting with `//` are ignored. Files live in `testdata/corpus/<suite>.jsonl`.
The corpus is valid against exactly one upstream revision; record it in
`testdata/corpus/PIN` (the Switchyard commit sha the oracle was built from).

```jsonc
{
  // Stable identity. Never reuse, never renumber. Used in diff reports.
  "case_id": "stage/under-threshold-003",

  // Conformance tier: 1 = decision core, 2 = +LLM judge, 3 = +stage/tool-signals,
  // 0 = JEV/chat protocol only. Lets CI run a subset.
  "tier": 3,

  // Profile: exactly what a tenant selects. `algorithm` + algorithm config only.
  // No model pool here — that is per request (see `models`).
  "profile": {
    "algorithm": "stage_router",
    "picker": "efficient_first",
    "confidence_threshold": 0.5,
    "recent_window": 3,
    "only_on_wrong_signal_escalation": true,
    "escalation_note": "The efficient tier failed; continue from its work.",
    "deescalation_note": "The capable tier completed the recovery.",
    "capable_system_prompt": "Handle this request as the capable tier.",
    "efficient_system_prompt": "Handle this request as the efficient tier."
    // llm_fallback omitted => fully local, zero host calls
  },

  // Per-request model pool, best-first. Categories: any|capable|efficient|judge
  // plus deployment-defined names. Mirrors RuntimeModels.
  "models": {
    "capable":   ["anthropic/claude-opus-4-7"],
    "efficient": ["anthropic/claude-haiku-4-5"],
    "judge":     ["jev-latest"]
  },
  "subagent_models": null,          // optional; RuntimeModels::with_subagent

  // Headers normalized by Metadata::from_headers. Session/agent identity here is
  // what keys session state and affinity.
  "headers": {
    "x-switchyard-session-id": "s-1",
    "x-switchyard-agent-id":  "agent-1"
  },

  // The routing input. `format` + `body` is the raw provider payload; the Rust
  // side normalizes via switchyard-translation (decode_request). Alternatively
  // supply `llm_request` (already-normalized switchyard JSON) and omit body.
  "request": {
    "format": "openai_chat",
    "body": {
      "model": "auto",
      "messages": [
        { "role": "user", "content": "fix the failing test" },
        { "role": "assistant", "content": null, "tool_calls": [
          { "id": "c1", "type": "function",
            "function": { "name": "run_tests", "arguments": "{}" } } ] },
        { "role": "tool", "tool_call_id": "c1", "content": "2 passed; 1 failed" }
      ]
    }
  },

  // Scripted decider responses. Both sides must consume these, so the case is
  // hermetic and offline. Empty/omitted for local-only profiles — and then the
  // step diff asserting *zero* calls is the point of the case.
  "decider_responses": {
    "by_index": [
      { "kind": "decision", "response": { /* DecisionResponse JSON */ } }
    ],
    "by_request_hash": {
      "sha256:1f3c...": { "kind": "decision", "response": { /* ... */ } }
    }
  },

  // Optional per-case expectations, checked on the *oracle* result during record.
  // Guards against a fixture that silently stops exercising the intended path.
  "expect": {
    "step_kinds": ["CallDecision", "Done"],
    "evidence_source": "decision_classifier"
  }
}
```

### Session sequences

When a case needs multi-turn behaviour (affinity latch, fall-open persistence,
`capable_hold_turns`), use a sequence object instead of a single request:

```jsonc
{
  "case_id": "affinity/latch-across-turns-001",
  "tier": 1,
  "profile": { "algorithm": "llm_classifier", "mode": "capability",
               "judge": { "type": "decision", "cutoff": 0.4,
                          "candidates": { "a": "quality", "b": "economy" } } },
  "models": { "capable": ["quality"], "efficient": ["economy"], "judge": ["jev-latest"] },
  "turns": [
    { "headers": { "x-switchyard-session-id": "s-1" },
      "request": { "format": "openai_chat", "body": { "messages": [{ "role": "user", "content": "hi" }] } },
      "decider_responses": { "by_index": [{ "kind": "decision", "response": { } }] } },
    { "headers": { "x-switchyard-session-id": "s-1" },
      "request": { "format": "openai_chat", "body": { "messages": [{ "role": "user", "content": "again" }] } },
      "decider_responses": { "by_index": [] } }   // must not re-judge: latch proves it
  ],
  // Session state is NOT reset between turns; it IS reset before turn 0.
  "reset_session": true
}
```

### Fixture matching

Responses are matched **by step index first** (authoritative, deterministic).
`by_request_hash` (sha256 of the canonical `DecisionRequest`/`Request` JSON) is
also computed and recorded; a *miss* there means the cascade diverged before the
call and is reported as `fixture_hash_miss` — a useful early divergence signal,
not a failure on its own.

A case that emits a step with no scripted response is a **hard error**: the
implementation called out when it should not have. This is how "local profile
must make zero calls" is enforced.

---

## 3. Fields to diff

Two result records are emitted per case (oracle, impl), same schema:

```jsonc
{
  "case_id": "...",
  "turns": [
    {
      "steps": ["CallDecision", "Done"],        // ordered Step kinds
      "step_count": 2,
      "selected_model_ids": ["quality", "economy"],   // ordered, best-first
      "selected_model_id": "quality",                 // head of the list
      "rewritten_model": "quality",                   // request.llm_request.model after routing
      "request_rewrite_digest": "sha256:...",         // instructions/system-prompt/notes delta
      "evidence": { "source": "decision_classifier", "verdict": "relative_advantage",
                    "score": 0.83, "threshold": 0.4 },
      "algorithm": "llm_classifier",
      "decision_source": "override",                  // State.extra["decision_source"]
      "affinity_key": "session:s-1",                  // RoutingIdentity, resolved
      "affinity_latched": ["s-1 -> quality"],         // effective assignment table
      "session_state": {                              // State dump (see §5)
        "turn_count": 1,
        "tool_signals": { "observe": 0, "mutate": 1, "plan": 0, "new": 0 },
        "extra": { "decision_source": "override" }
      },
      "outcome_id": "MASKED",
      "durations": "MASKED"
    }
  ]
}
```

| Field | Why it matters | Diff policy |
|---|---|---|
| `steps` / `step_count` | proves local-vs-remote: a local profile must emit no `CallModel`/`CallDecision` | exact |
| `selected_model_ids` | the decision, **including fallback ordering** (selected → rest of its category → other targets) | exact, ordered |
| `rewritten_model` | the model stamped into the request | exact |
| `request_rewrite_digest` | tier system prompts and handoff notes injected by processors | exact |
| `evidence` | `{source, verdict, score, threshold}`; fail-open emits `{source:"fail_open", reason_code}` | exact, key order ignored |
| `algorithm` | `Algorithm::name` | exact |
| `decision_source` | `Override` / `Dimensions` / `FallOpen` / `CapableHold` / `Ambiguous` | exact |
| `affinity_key` | resolved `RoutingIdentity` (session, or session+agent for subagents) | exact |
| `affinity_latched` | the retained assignment table after the turn | exact |
| `session_state.turn_count`, `.extra` | persistence + fall-open / capable-hold counters | exact |
| `session_state.tool_signals` | the signal scoring input | exact |
| `outcome_id` | UUIDv7, time-based | **masked** |
| `durations` | `Instant`-derived | **masked** |

Diagnostics allowed to differ (reported, not failed): log lines, `tracing`
spans, metric counters.

---

## 4. Runner architecture

Two runners, one shared corpus and result schema.

### Rust oracle runner

A **plain bin**, e.g. `cargo run -p libsy-conform`, linking `libsy`,
`switchyard-protocol`, and `switchyard-translation` directly — no FFI, no shim.
Pin it to the revision in `testdata/corpus/PIN`.

```sh
libsy-conform --corpus testdata/corpus --mode oracle --out out/oracle.jsonl
```

Internally: parse case → build `Algorithm` from `profile` → build
`RuntimeModels` from `models` → for each turn, `run_stream`, serve each step from
`decider_responses.by_index`, collect `Step::Done` outcome and serialise `State`
in-process.

Because the oracle runs in-process, `State` and the affinity table are read with
no ABI at all. That is the point: the oracle's JSON is the reference, and the Go
side (§4) is written to match it — not the other way round.

### Go runner

Same CLI, same schema:

```sh
go run ./cmd/libsy-conform --corpus testdata/corpus --mode impl --out out/go.jsonl
```

### Modes

| Mode | Action |
|---|---|
| `record` | oracle only; write result JSONL; refuses to overwrite fixtures unless `--force` |
| `verify` | both runners; diff; exit non-zero on any field diff |
| `bless` | rewrite expected results as the new golden (requires review) |
| `subset` | filter by `--tier` and `--case` glob for fast local iteration |

```sh
# full round trip
libsy-conform --corpus testdata/corpus --mode verify
# diff detail
libsy-conform ... --mode verify --report out/diff.json --verbose
```

### Comparator

Line-by-line over the two result JSONLs matched by `case_id` + `turn` index.
Reports:

- `mismatch` — a diffed field differs (with JSON Pointer to the exact path).
- `missing_case` / `extra_case` — corpus coverage drift.
- `fixture_hash_miss` — cascade diverged before a call.
- `unexpected_call` — a step with no scripted response (hard failure).
- `masked_field_diff` — a masked field differed (warning only; catches accidental
  leakage of nondeterminism into a diffed field).

Exit non-zero on any `mismatch`, `unexpected_call`, `missing_case`, or
`extra_case`.

---

## 5. State exposure

The diff includes session `State` (§3) because much of the tuned behaviour lives
there rather than in the visible model choice — `decision_source`, `fall_open`,
`capable_hold`, `turn_count`, and `tool_signals`.

**This is an upstream API gap, not a shim task.** `State` is unreachable from
outside libsy today:

- `FallThrough` is not exported from the crate root, and its `session_states`
  field is private with no accessor.
- `ToolSignalProcessor` is likewise unexported.
- `State` / `StateValue` are not `Serialize`.
- `RoutingOutcome` carries no state, and `Algorithm` has no state hook.

So neither the oracle bin nor a future C-ABI shim can reach it — a shim has
nothing to call. Three ways forward:

**A. Upstream accessor (preferred).** Measured at ~40 lines of core change plus a
runnable example: 103 insertions / 4 deletions across 7 files. See
`patches/0001-expose-session-state.patch`, which is verified to apply and build.

- `State` / `StateValue` / `ToolSignals` gain `Serialize`.
- `Algorithm` gains a defaulted method (non-breaking for other implementors):
  `async fn state_snapshot(&self, session_id: &str) -> Option<Value>`.
- `FallThrough<S>` gains an inherent `session_state_json(&self, session_id)`;
  `StageRouter` and `LlmTaskClassifier` delegate to it.

The one non-obvious detail: session state sits behind a `tokio::sync::Mutex`, so
the accessor must be `async` (or contention-fragile via `try_lock`). That also
carries into the shim, where `sy_run_state_dump` would `block_on` it.

**B. Vendor a fork.** If upstream declines, point a
`[patch."https://github.com/NVIDIA-NeMo/Switchyard.git"]` at a fork carrying the
same change, and rebase it on each pin bump.

**C. Infer from behaviour (no upstream change).** Every state field has an
observable consequence, so a corpus can be built to make each one load-bearing:
`fall_open` is already visible via `evidence.source`; the affinity latch via
`unconsumed_scripted` across turns; `capable_hold_turns` and the
`decision_source` variants via the tier transition index in a crafted sequence;
`tool_signals` via threshold-straddling requests. This costs corpus cases rather
than code, and localises a regression less precisely — you see "turn 4 tier
differs", not "hold counter off by one".

**Importance — proportional to how much you own.**

- **JEV / chat path (tiers 0–1): minor.** Everything that matters is already
  observable: tier, ordered candidates, `evidence` (`source` / `score` /
  `threshold` / `reason_code`), and the rewritten request. Ship without it.
- **`stage_router` (tier 3): significant.** `tool_signals` is the largest and
  most churn-prone surface to reimplement (~1.7k prod LOC), and without a
  score-level diff a scoring regression is invisible until it flips a threshold —
  so you need many cases to localise it. If you port tier 3, take option A first:
  it is what makes that port verifiable rather than merely plausible.
- **If you bind rather than port: the gap does not exist.** Rust owns the state
  and libsy's own test suite covers it. The caveat is a consequence of owning
  routing outside Rust.

`tool_signals` is a flat struct of primitives (`f32` / `u32` / `bool`), so
serialising the whole thing is cheap and turns a scoring regression into a diff
rather than a downstream tier change.

---

## 6. Conformance tiers

Corpus cases are tagged so CI can gate progressively.

| Tier | Covers | Modules on the Go side |
|---|---|---|
| 0 | JEV / chat wire protocol only (encode request, decode verdict, cutoff) | JEV client, `DecisionRequest`/`Response` |
| 1 | decision core: cascade, affinity, ambiguity, fail-open, candidate ordering | `fall_through`, `affinity`, `llm_class/decision` |
| 2 | + LLM-judge variant | `classifier_contract` (JSON-Schema verdict validation) |
| 3 | + `stage_router`: tool signals, escalation, capable-hold | `tool_signals` (1,661 prod LOC), `util/stage` |

Tier 3 is where the port is expensive and drift is most likely; it should have
the densest corpus.

---

## 7. Corpus authoring guidance

- **Every tier-3 case must be churn-sensitive.** Cover: under-threshold abstain →
  `FallOpen` → default tier; `Override`; `Dimensions`; `CapableHold` decrement and
  clear-on-pass; escalation and de-escalation notes; tier system prompts.
- **Boundary cases for tool signals**: a turn with no tool activity
  (`tool_signals = None`); a tool burst crossing `recent_window`; Anthropic-style
  `Role::User` tool results (must not count as a user turn).
- **Ambiguity and invalid verdicts**: `Classification::Ambiguous` abstains; NaN
  confidence is an error; a missing/invalid `advantage` probability → `fail_open`
  with `reason_code: "invalid_verdict"`; `fail_open = false` propagates.
- **Affinity**: same session latches; new session re-judges; `session_final`
  evicts; subagent refines the key to session+agent; `latch_only` refuses to latch
  the weak tier.
- **Ordering**: assert the full `selected_model_ids` for a chain/split so the
  fallback ordering is pinned, not just the head.
- **Local-only proof**: at least one case per local profile asserting
  `step_count` is `["Done"]` — the regression that matters most for owning HTTP
  in Envoy (a local profile must never reach the network).
- **Live captures over invented numbers, where it matters.** A judgement call has
  no defensible synthetic value; capture it from the real decider once and pin it
  with provenance (`testdata/fixtures/<provider>/<date>-<case>.json`). Keep the
  synthetic cases only for boundaries a live model will not reliably produce
  (exact-cutoff, missing-score, malformed verdict). Recorded in this repo: a real
  JEV call returning `advantage = 0.25` against a `0.4` cutoff.

  Capturing is a separate step from verifying — live calls are not deterministic,
  so `record` may write fixtures but `verify` must always replay them.

---

## 8. CI and acceptance

```sh
# oracle pinned to the commit in testdata/corpus/PIN
cargo test -p libsy                       # upstream's own suite still green
libsy-conform --corpus testdata/corpus --mode verify
```

Acceptance for swapping in the Go implementation behind the same profile API:

1. `verify` is green across all tiers, on the full corpus.
2. Every tier-3 case emits identical `tool_signals` and `decision_source`.
3. No `unexpected_call` for any local-only profile.
4. No diff in `selected_model_ids` ordering.
5. `by_request_hash` fixtures hit for every case that calls out.

### Drift check

`scripts/drift-check.sh` re-pins to upstream main in a scratch checkout, records a
golden at that revision, and answers two questions that are not the same question:

| | Compared | Answers |
|---|---|---|
| **A** | oracle@latest vs golden@pinned | did upstream behaviour move? |
| **B** | port@here vs golden@latest | did *our port* fall behind? |
| **C** | golden@pinned vs golden@latest | which recorded turns moved, and how |

A is the news. B is the actionable number: it names the turns libsygo would now
route differently, which is the thing that decides whether owning routing outside
Rust stays cheap. Reporting only A tells you upstream moved without telling you
whether it matters here; reporting only B hides movement in families the port has
not implemented yet — which is why the tier-1 filter on B is deliberate, and why C
is reported unfiltered.

A compile failure is reported as *cannot be evaluated*, distinct from behavioural
drift: it means upstream changed API shape, so the harness and corpus must be
updated before either question below it has an answer.

The job runs weekly and is also the manual gate for taking a pin bump:
