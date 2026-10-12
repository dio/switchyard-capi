# Upstream patches

`0001-expose-session-state.patch` — exposes libsy's retained per-session routing
`State`, so a host (the harness, or a future C-ABI shim) can diff it.

**Why it's needed.** `State` is unreachable from outside libsy today: `FallThrough`
is not exported, its `session_states` field is private, `ToolSignalProcessor` is
unexported, `State`/`StateValue` are not `Serialize`, and neither `RoutingOutcome`
nor the `Algorithm` trait carries state. A C-ABI shim therefore has nothing to
call — the caveat is an API gap, not an FFI task.

**Size.** 103 insertions / 4 deletions across 7 files, of which 64 lines are the
runnable example. The core change is ~40 lines:

- `State` / `StateValue` / `ToolSignals`: derive `Serialize`.
- `Algorithm`: a defaulted (non-breaking) `async fn state_snapshot(&self, session_id: &str) -> Option<Value>`.
- `FallThrough<S>`: inherent `session_state_json`; `StageRouter` and
  `LlmTaskClassifier` delegate. The accessor is `async` because session state sits
  behind a `tokio::sync::Mutex`.

Verified against the pinned revision in `../libsygo/testdata/corpus/PIN`:

```sh
cd /path/to/Switchyard
git apply --check /path/to/switchyard-capi/patches/0001-expose-session-state.patch
git apply          /path/to/switchyard-capi/patches/0001-expose-session-state.patch
cargo run -p switchyard-libsy --example state_snapshot
# -> {"turn_count":0,"tool_signals":{...},"extra":{"decision_source":{"String":"fall_open"}}}
```

## Applying it to this repo

Until the patch is upstream, point the dependency at a fork via `Cargo.toml`:

```toml
[patch."https://github.com/NVIDIA-NeMo/Switchyard.git"]
switchyard-libsy = { git = "https://github.com/<fork>/Switchyard.git", rev = "<rev>" }
```

Rebase the patch on each pin bump. If upstream declines, option C in
`docs/differential-harness.md` §5 (infer state from behaviour) is the fallback —
it costs corpus cases instead of a fork, and localises regressions less precisely.
