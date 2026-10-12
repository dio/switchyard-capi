# C ABI sizing

Measured, not extrapolated. Built `crates/switchyard-capi` — the pull loop plus
two kinds (one DTO-free, one needing a DTO), with four integration tests — and
counted what it cost. Reproduce with `cargo test -p switchyard-capi`.

## Method

The surface was censused from the pinned source, then the fixed parts were
measured by building them, so the estimate rests on two real numbers rather than
one assumption.

## The census

21 config/enum types, 66 fields. **Only 12 types need a DTO:**

| Bucket | Count | Types |
|---|---|---|
| Already `Deserialize` | 8 | `EscalationJudgeConfig`, `DeescalationConfig`, `ClassifierContractConfig`, `HandoffNoteConfig`, `ToolSemantics`, `PickerMode`, `ClassifyTrigger`, `ClassifierResponseFormat` |
| Manual `Deserialize` | 1 | `TaskClassifierConfig` |
| Needs a hand-written DTO | 12 | `AdvisorGateConfig`, `PlanExecuteConfig`, `SubagentRouterConfig`, `CompositeRouterConfig`, `StageRouterConfig`, `CustomClassifierConfig`, `LlmCapabilityConfig`, `DecisionJudgeConfig`, `LlmClassifierConfig`, `CapabilityJudgeConfig`, `CustomClassifierPolicy`, `GateTrigger` |

So ~43% of the surface costs nothing to decode, and only ~49 of 66 fields need
explicit mapping.

## Measured constants

| Constant | Lines | Where |
|---|---|---|
| Pull loop (`sy_run_start`/`sy_run_next`/`encode_step`/`sy_call_respond`/`sy_call_fail`) | **182** | `src/lib.rs` |
| Construction, freeing, input decoding | 185 | `src/lib.rs` |
| **Fixed core, 2 kinds** | **367** | `src/lib.rs` |
| **One DTO** — 8 fields across 4 nested libsy types | **51** | `src/config.rs` (29 struct, 22 mapping) |
| Tests for all of it | 121 | `tests/abi.rs` |

The DTO fits `20 + 4 × fields`: 20 for the struct shell, ~4 per field. Measured 51
for 8 fields, i.e. ~4.0 lines per field — the assumption the estimate depends on.

## Estimate for all 11 families

| Component | Lines | Basis |
|---|---|---|
| Fixed core (loop, handles, decoding, freeing) | 367 | measured |
| 9 further kind dispatches | 54 | ~6 each, measured on the 2 present |
| DTO layer: 8 structs (44 fields) + 4 enums | ~400 | `20 + 4×fields`, measured |
| `sy_state_snapshot` (+ the ~40-line upstream patch) | 70 | needs `0001-expose-session-state.patch` |
| Error mapping fidelity | 30 | |
| **Rust ABI** | **~0.9k** | |
| Go wrapper (12 DTOs, 11 constructors, pull loop, cgo) | ~420 | mirrored |
| **Total** | **~1.3k** | |

Against porting: ~7–9k Go, plus re-deriving the 10.3k LOC of upstream tests as a
conformance corpus, plus owning a fork of the semantics. Binding also gets
`tool_signals.rs` (1,661 lines) and `jsonschema` verdict validation for free —
the two items that made full porting unattractive.

## Incidental findings

Three things the skeleton surfaced that a Go wrapper must know:

1. **The neutral IR nests its tagged enums.** A decision question is
   `questions.route.kind.type`, not `route.type` — the IR differs in shape from
   System One's flat wire form. A wrapper that assumes the wire shape will read
   `null`.
2. **libsy does not validate the model pool at construction.** A pool missing
   `any` is accepted and fails later as `TargetNotFound` mid-route. Both this ABI
   and the Go port validate early instead, so the two agree under the differential
   harness — and because that is an addition, it is commented as one.
3. **`crate-type` needs `rlib`.** With only `staticlib`/`cdylib`, the ABI's own
   integration tests cannot link it. (`switchyard-py` carries `rlib` for the same
   reason.)
4. **Hash libsy's bytes; do not re-encode.** Anything derived from a returned
   request (here, the conversation digest) must be computed over the exact bytes
   libsy serialized. A wrapper that decodes into a map and re-marshals gets a
   different field order — Go sorts map keys, so `{"role":…,"content":…}` becomes
   `{"content":…,"role":…}` — and therefore a different digest:

   ```
   golden digest          sha256:7b247019ae91cc77…
   libsy bytes, hashed    sha256:7b247019ae91cc77…   match
   Go re-encoded, hashed  sha256:329d99ba902e8811…   differs
   ```

   Decode into `json.RawMessage` for those fields, not into a structure.
5. **A JSON producer emits `null` for an absent map, and `#[serde(default)]` does
   not cover it** — only a missing key. The ABI therefore takes optional maps as
   `Option<BTreeMap<…>>`. This is the first bug the ABI backend hit, and it is the
   kind of thing only an end-to-end diff finds.

## Caveats

- Only 2 of 11 kinds are implemented, so the per-kind dispatch cost is measured on
  a small sample.
- Streamed `CallModel` responses are not handled — only aggregate replies. Adding
  streams would raise the loop's cost.
- The estimate assumes the remaining families' configs are shaped like this one.
  `advisor_gate` (10 fields, plus a `GateTrigger` enum) and `stage_router` (9
  fields, nested `ToolSemantics` + `HandoffNoteConfig`) are the largest; neither is
  obviously worse.
- `SubagentRouter::new(parent, config)` needs handle composition — an algorithm
  referencing another — which is not exercised here and is the one structurally
  new piece.
