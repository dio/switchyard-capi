# Profile contract

**Status:** Draft for review. Defines what a *profile* is, who owns it, and how the
dataplane resolves one per request — the contract between tenant policy and the
routing decider.

Companion to `docs/differential-harness.md`. The decider implementation this
targets lives in `github.com/dio/switchyard-capi` (tier-0/1 Go port), verified
turn-for-turn against the Switchyard oracle.

---

## 1. Why

A tenant must be able to choose *how* their traffic is routed, per request, in a
multi-tenant dataplane. That choice is a **profile**: the algorithm, its
configuration, the model pool it may route over, and the judge it consults.

Three properties drive the shape:

- **Per request, not per process.** The same gateway serves many tenants; the
  algorithm and pool change per request. (libsy is already built this way — models
  are passed per `run_stream`, and the same algorithm object can route against a
  different pool on every call.)
- **No new namespace.** The dataplane already has a client-facing model catalog.
  A profile must reference *those* ids, not invent a parallel one.
- **No cascade at request time.** Materialization (inheritance, defaults, merges)
  is a control-plane concern. The dataplane receives a self-contained profile and
  serves it.

---

## 2. What a profile is

```
profile = algorithm + config        (how to decide)
        + model pool by category    (what may be chosen)
        + judge binding             (where the verdict comes from)
        + revision                  (so a decision is reproducible)
```

A profile is **opaque to the dataplane**: it is authored, validated, and
materialized upstream, and arrives whole. The dataplane does one lookup and
serves it.

---

## 3. Shape

This is the shape the tier-0/1 port consumes today (see `testdata/corpus/*.jsonl`
for live examples). Production adds `id`, `revision`, and a judge binding.

```jsonc
{
  "id": "ws-prod/user-maya/balanced",
  "revision": 7,                       // bumped on every materialization

  "algorithm": "llm_classifier",       // llm_classifier | random | noop | stage_router
  "mode": "capability",                // capability (others not ported)

  "judge": {
    "type": "decision",                // decision (JEV / System One). llm judge: tier 2
    "binding": "jev-default",          // named judge endpoint + credential (see §6)
    "cutoff": 0.4,                     // route capable only when advantage > cutoff
    "candidates": { "a": "smart", "b": "cheap" },
    "evidence": {                      // passed to the judge unchanged
      "candidates": { "a": "Frontier model", "b": "Fast low-cost model" }
    }
    // "instructions": {...}           // optional; defaults to the packaged prompt
  },

  "fail_open": true,                   // judge failure → default tier (§8)
  "session_affinity": true             // latch a target per session (§7)
}
```

**Model pool**, per request, in the port's category vocabulary:

```jsonc
{
  "capable":   ["smart"],
  "efficient": ["cheap"],
  "judge":     ["jev-latest"],
  "any":       ["cheap", "smart"]     // required: see §4
}
```

| Field | Meaning |
|---|---|
| `capable` / `efficient` | The two tiers the capability classifier chooses between, best-first. |
| `judge` | Model ids the decider may call to decide. Never a routing destination. |
| `any` | The routable pool. The selected target is resolved against it. |
| `id` / `revision` | Materialization identity; part of the cache key. |

---

## 4. Lowering: referencing models

**Ids are dataplane model ids** — the same namespace as the client-facing catalog
(the request's `model` field). A profile does not carry provider, endpoint, or
credential information; resolving a chosen id to an upstream stays the
dataplane's existing business.

That composition is deliberate:

```
request → profile → decider → "smart"
                             → the dataplane's existing model resolution
                               (chain / split / binding) → upstream
```

The decider picks *which alias*, not *which host*. Failover, splitting, and
credential injection are unchanged and keep working.

### The `any` invariant

`any` must list **every** routable target, and every other category must be a
subset of it. This is not a convention — libsy resolves the selected target
against `any` and errors with `TargetNotFound` otherwise:

```
target "smart" was not found
```

The orchestrator derives it rather than trusting authors:

- `llm_classifier` (capability): `any = [efficient..., capable...]`
- `stage_router`: `any = [capable..., efficient...]`

Order matters — `any` is the fallback order after the chosen tier is exhausted, so
it is part of the contract, not an implementation detail.

---

## 5. How a request opts in

Routing must be **opt-in per request**, or every tenant's existing model names
change meaning. The mechanism already exists: the client-facing catalog can hold
router entries, and the request's `model` field selects one.

```
"model": "switchyard/balanced"   → profile ws-prod/user-maya/balanced
"model": "claude-sonnet-4"       → no profile; existing passthrough behaviour
```

So the profile is keyed by `(tenant, router id)`, and a request that does not name
a router id is untouched. This mirrors how the reference runner exposes routes
(`id = "switchyard/stage"`).

---

## 6. Judge binding

The judge is a **separate contract** from the model pool, because it is not a
routing destination and its credentials are not the tenant's:

| Property | Owner | Why |
|---|---|---|
| Endpoint + credential | platform | Caller credentials are never forwarded to the decision provider. |
| Judge model id | profile (`judge` category) | Lets a tenant pin a judge version without a platform change. |
| Timeout | platform | Bounded, and part of the request's latency budget. |
| Instructions / evidence | profile | Evaluation-specific; authored alongside the cutoff. |

Consequences:

- Credentials live in the dataplane's secret handling, never in the profile blob.
- A profile names a **binding**, not a URL or key.
- The judge call is an ordinary egress call, so the dataplane owns TLS, retries,
  and the timeout — which is why the port does not link an HTTP stack.

---

## 7. Session identity and multi-tenancy

Per-session state (the latch) is keyed by session id, and the port keys it by
`x-switchyard-session-id`. Two tenants can collide on that value.

**Requirement:** namespace session identity with the tenant before it reaches the
decider — `session_key = tenant_id + "/" + session_id` — or key the latch table by
`(tenant, session)`. Without this, one tenant's latched target can be served to
another.

Sessions must also be evictable; the dataplane should honour a session-final
signal so a long-lived gateway does not accumulate state.

---

## 8. Failure semantics

| Situation | Behaviour |
|---|---|
| Judge returns a verdict | `advantage > cutoff` → capable, else efficient. Strictly greater: **at** the cutoff is not capable. |
| Judge returns no usable score | `invalid_verdict` → `fail_open` (§below). |
| Judge call fails or times out | `call_error` → `fail_open`. |
| `fail_open: true` (default) | Route to the **capable** tier and record the failure. Safe, expensive. |
| `fail_open: false` | Propagate the error; the request fails rather than mis-routes. |

`fail_open` landing on *capable* is libsy's behaviour (the cascade closes on the
capable category), not a dataplane policy. A tenant that would rather fail than
pay should set `fail_open: false`; a tenant that would rather degrade than fail
should leave it true and budget for it.

Every decision carries evidence (`{source, verdict, score, threshold}` or
`{source: "fail_open", reason_code}`), which is what makes a decision explainable
after the fact — and what the conformance harness diffs.

---

## 9. Latency

The judge call is a real round trip in the request path: measured 70–500 ms for
System One, versus single-digit milliseconds for the signal-only algorithms.

Therefore the decision **must be asynchronous** — the dataplane already has the
primitives (pause the stream, run the callout off-worker, resume). Two
consequences for the contract:

- The profile carries a timeout, and it is part of the request's deadline budget.
- A profile whose algorithm makes no judge call (a local profile) must be
  expressible, so a tenant can opt into zero added latency.

---

## 10. Materialization and ownership

| Concern | Owner |
|---|---|
| Authoring, validation, inheritance, defaults | Control plane |
| Deciding it is a *router* entry in the catalog | Control plane |
| Per-request resolution, calling the judge, choosing a target | Dataplane |
| Resolving the chosen id to an upstream, credentials, retries | Dataplane (existing) |
| Judge endpoint + credential | Platform |

The dataplane does no cascade, no merge, and no inheritance. If it needs a value,
the materializer put it there.

**Caching:** profiles are resolved per request, but the *built decider* is
immutable per `(tenant, profile id, revision)`. Cache the built handle on that
triple; a revision bump naturally produces a new entry, and the old one drains.

---

## 11. Worked example

```yaml
# Materialized for ws-prod / user-maya. Served whole; the dataplane does not
# cascade this with anything.
profiles:
  ws-prod/user-maya/balanced:
    revision: 7
    client_model: switchyard/balanced     # the id clients send in "model"

    algorithm: llm_classifier
    mode: capability
    judge:
      type: decision
      binding: jev-default
      cutoff: 0.4
      candidates: { a: smart, b: cheap }
      evidence:
        candidates:
          a: Frontier model, best for repository-scale changes
          b: Fast low-cost model, fine for edits and questions
    fail_open: true
    session_affinity: true

    pool:
      capable:   [smart]
      efficient: [cheap]
      judge:     [jev-latest]
      # any is derived: [cheap, smart]
```

Observed against the live judge:

| Request | advantage | Decision |
|---|---|---|
| "write a haiku" | 0.02–0.28 | efficient (`cheap`) |
| "Refactor the payment module onto the new ledger abstraction, migrate every caller, and get the full integration suite green…" | **0.77** | capable (`smart`) |

Both are pinned as fixtures with provenance in
`switchyard-capi/testdata/fixtures/jev/`.

---

## 12. Open questions

1. **Candidate labels.** `candidates` exists so the judge can talk about models
   without brand names. Should labels be authored, or derived canonically
   (`a` = capable, `b` = efficient)? Deriving them makes profiles smaller and
   diffs stabler; authoring them allows the evidence text to stay meaningful.
2. **Per-tenant judge model.** Should a tenant be able to pin `jev-latest` →
   `jev-1.13.0` for reproducibility? (Yes is cheap; it only changes one id.)
3. **Cost/latency ceiling.** A judge call per turn is the dominant added cost for
   a high-turn agent. Should the profile carry a per-session decision budget, or
   is that the control plane's problem?
4. **Revision semantics.** Is `revision` a materialization counter, or does it
   carry semantic meaning the dataplane acts on (e.g. drain in-flight sessions)?
5. **Non-router profiles.** Should signal-driven routing (no judge, no network) be
   selectable through the same contract? It fits the shape, but the port does not
   implement it — see the tier-3 note in `docs/differential-harness.md` §5.
