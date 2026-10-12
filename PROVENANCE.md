# Provenance

## Embedded judge prompt

`go/internal/conform/prompts/relative_advantage.json` is copied **verbatim** from
upstream at the revision in `testdata/corpus/PIN`:

```
crates/libsy/src/prompts/capability-classifier/relative_advantage.json
sha256 ce45c569069fb51b3570c9faf4b7c519106a2076437ed98a5c992e67d9b6df24
```

It is embedded with `go:embed` and used when a profile does not supply its own
judge instructions. It is **data**, so it drifts with upstream: the drift job
(`scripts/drift-check.sh`) re-records against newer revisions, and this file must
be re-copied whenever the pin moves.

## Live captures

`testdata/fixtures/jev/*.json` are recorded from the real System One endpoint,
with the request, model, cutoff, and outcome. They are observations, not
specifications — re-recording may change them as the served model moves.

Capture and verification are deliberately separate. Live calls are not
deterministic (`verify` must always replay pinned fixtures), so:

- **Capture** is manual, needs `TYPESAFE_API_KEY`, and writes the corpus's
  scripted-reply shape so a capture can be pasted straight into
  `decider_responses.by_index`.
- **Verify** never touches the network.

Recorded so far:

| Fixture | Outcome |
|---|---|
| `2026-10-12-choice-no-advantage.json` | `advantage 0.25 < 0.4` → efficient |
| `2026-10-12-realistic-hard-task.json` | `advantage 0.77 > 0.4` → capable |
