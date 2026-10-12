# What libsy is — and what it is worth taking

Written to answer one question: is libsy a **bundle of algorithms** (in which case
porting to Go is coherent), or is it entangled with **peripheral parsers and
transformers** (in which case take only the core)? The answer is mostly the
former, with two exceptions, and both are confined.

## The crate boundary already draws the line

`switchyard-libsy` depends on exactly one Switchyard crate:

```
switchyard-protocol   the shared vocabulary (Request, LlmRequest, Message,
                      ToolCall, DecisionRequest/Response, ModelId, …)
```

It does **not** depend on `switchyard-translation`, `switchyard-llm-client`,
`switchyard-runner`, or `switchyard-server`. So the things usually described as
peripheral are — already — not inside it:

| Peripheral | Where it actually lives | Do we need it? |
|---|---|---|
| Provider wire parsing / transforming (OpenAI ↔ Anthropic ↔ neutral) | `switchyard-translation` | **Own it.** Separate crate, pure Rust, no HTTP. |
| HTTP, retries, fallback, streaming, the System One codec | `switchyard-llm-client` | **Owned.** Envoy does the transport; `libsygo/jev` already owns the System One codec. |
| TOML config → routes/clients | `switchyard-runner` | Not needed; a materializer ships profiles whole. |
| HTTP endpoints | `switchyard-server` (upstream calls it "Demo") | Not needed. |

`libsy` itself is the algorithms plus the host contract. **"Take the core" and
"take libsy" are the same instruction** — which is why "port the core" was always
the right phrasing.

## The bundle: what the algorithms actually do

Production lines, and what each family buys you.

| Family | What it does | Rust prod | Needs network? | Needs tools/messages? |
|---|---|---|---|---|
| **Composition** — `fall_through`, `affinity` | The cascade and the latch: processors fold facts into per-session state; classifiers are consulted in order; first to score decides; the pick re-orders fallbacks | 580 | no | no |
| **Capability classifier** — `llm_class`, `llm_judge`, `decision`, `classifier_contract` | Asks a judge whether the cheap tier can do the task; LLM-text verdict *or* typed decision verdict; JSON-Schema verdict validation | 1,647 | **yes** | no |
| **Signal router** — `tool_signals`, `stage`, `util/stage` | Reads tool activity and outcome signals (errors, streaks, mutation intensity) and picks a tier without any call | 2,610 | no | **yes** |
| **Escalation** — `escalation`, `util/escalation` | Judges *responses*, escalating after a confirmed streak of failures; de-escalates after sustained success | 1,012 | **yes** | yes |
| **Advisor gate** — `advisor_gate/*` | A stronger model reviews the weaker one's work before it is accepted; budgeted, gated on stall/tool signals | 668 | **yes** | yes |
| **Plan / execute** — `plan_execute` | One model plans, another executes | 212 | **yes** | yes |
| **Composite** | Capability classifier sets the default tier; stage router handles the rest | 172 | **yes** | yes |
| **Sub-agent** — `subagent`, `util/subagent` | Routes delegated child-agent work independently of the parent | 286 | inherits | yes |
| **Trivia** — `rand`, `passthrough`, `noop` | Weighted random, fixed target, no-op | 258 | no | no |
| **Host contract** — `core/algorithm`, `classifier`, `processor`, `state` | `Algorithm`, `Step`/`CallModel`/`CallDecision`, `Driver`, `RuntimeModels`, `RoutingOutcome` | 928 | — | — |

Roughly **8.7k of the 10.9k** is the algorithm bundle; the remainder is
`observability` (321), test-only modules that do not ship (`core/testing`,
`advisor_gate/tests`, `tier_fixtures` ≈ 1.7k), and small utilities.

## The unique value is not any one algorithm

No single entry above is remarkable. What is distinctive is three things, in
order:

1. **The composition.** A processor chain folding facts into per-session state, a
   classifier cascade where the first to score wins, and a latch that makes a
   session sticky — with the selected target replayed to the processors so
   stateful ones can bind it. That is ~580 lines and it is the actual product.
2. **The accumulated tuning.** `capable_hold_turns`, `confidence_threshold`, the
   tool vocabulary and per-tool semantics, the escalation streak lengths. This is
   measured against benchmarks, not derivable, and it is why porting is risky —
   you can copy the structure and still route worse.
3. **The evidence discipline.** Every decision carries `{source, verdict, score,
   threshold}` or a fail-open reason, so a routing choice is auditable after the
   fact rather than observable only as which upstream answered.

## The two embedded parsers

The only "peripheral" code inside libsy, both confined:

1. **A shell-command parser** — `tool_signals.rs:598` uses `shlex::split` plus a
   `shell_segments` splitter to walk a tool call's command and decide whether it
   retrieves, writes, or plans. Necessary because tool signals are derived from
   tool *invocations*, and a bash command has to be understood to be classified.
   Portable (Go has `regexp`; a shell-word splitter is small), but it is the one
   place where "port the signal router" gets harder than the line count suggests.
2. **Regex verdict and trigger parsing** — `advisor_gate/{transcript,trigger}.rs`
   (7 call sites) parse a model's text reply into a structured verdict, and match
   trigger patterns. The LLM-judge path additionally uses `jsonschema` +
   `jsonptr` for verdict validation, which is the highest-risk thing to
   reimplement. **The typed decision judge we use does not touch any of it.**

Both sit in the signal and advisor families. The decision-judge path — the part
built and verified so far — has neither.

## Normalization is already separable

`libsy` reads a normalized `LlmRequest` but does not parse one. Parsing lives in
`switchyard-translation`; the *vocabulary* lives in `switchyard-protocol`. So:

- **Ownable.** Taking `protocol`'s LLM types (or an equivalent) and doing the
  provider decode yourself is coherent — they are separate crates, not tangled
  into the algorithms.
- **But not optional.** Any family marked "needs tools/messages" reads tool calls
  and message content out of the normalized request. A host that hands libsy only
  a prompt cannot run the signal router, escalation, advisor, plan-execute,
  composite, or sub-agent. Right now that is every family except the judge path.

## Port or bind, per layer

| Layer | Own it? |
|---|---|
| Egress, retries, timeouts, TLS | **Owned** (Envoy). |
| System One / JEV codec | **Owned** (`libsygo/jev`, ported and live-verified). |
| Normalization | **Ownable** (separate crate + shared types). |
| Host contract (`Step`/`Drive`) | **Owned** — replaced with a synchronous `Route`. |
| Observability | **Owned** (Go OTel; libsy's spans do not cross the ABI). |
| **The algorithm bundle** | **This is the only contested layer.** |

So the decision reduces to one layer, and the choice there is:

- **Bind** — ~0.9k Rust ABI + ~420 Go wrapper (measured: the core is 367, a DTO is
  ~4 lines/field). Gets all families, and the two hardest items —
  `tool_signals` and `jsonschema` — for free.
- **Port** — ~7–9k Go, plus re-deriving 10.3k LOC of upstream tests as a corpus,
  plus owning a fork of the tuned constants. But: no Rust toolchain, no cgo, no
  C ABI versioning, and no drift — you own it permanently, which is the argument
  for doing it even at 5× the cost.

## The claim this supports

libsy is a **portable bundle of algorithms**, not an entangled framework — so
either path is coherent, and "port the core" was always well-formed. The
periphery you were right to want to own (normalization, transport, host loop,
observability) is either already outside libsy or already owned here.

What porting does *not* get you is the tuning, and that is the part no port can
copy: it is the reason the drift job exists and the reason the differential
harness — which diffs tier, ordered candidates, and evidence turn for turn — is
the thing that makes either choice safe.
