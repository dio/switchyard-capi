BIN := .bin/libsy-conform-impl

.PHONY: all build build-rust build-go cargo-test verify impl impl-capi go-test fmt-check clippy test live drift clean

all: test

build: build-rust build-go

build-rust:
	cargo build

build-go: build-rust
	mkdir -p .bin
	cd go && go build -o ../$(BIN) ./cmd/libsy-conform-impl

# Oracle vs recorded golden: proves the oracle is deterministic and upstream
# behaviour has not moved for the pinned revision.
verify:
	cargo run -q -p libsy-conform -- --mode verify

# The tier-0/1 Go port vs golden. The port covers llm_classifier + the decision
# judge only, so it is tier-filtered.
impl: build-go
	cargo run -q -p libsy-conform -- --mode impl --tier 1

# The same corpus and golden, driven through libsy's C ABI instead of the port.
# Two backends behind one flag is what makes a divergence visible rather than
# multiplied.
impl-capi: build-go
	LIBSY_CONFORM_BACKEND=capi cargo run -q -p libsy-conform -- --mode impl --tier 1

# The cgo package links the Rust staticlib, so the Rust build comes first.
go-test: build-rust
	cd go && go test ./...

# The Rust crates' own tests: the harness's, and the C ABI's pull-loop tests.
cargo-test:
	cargo test --workspace

# The example lives in its own module so the library stays dependency-free.
examples-test:
	cd examples/envoy-callout && go test ./...

fmt-check:
	cargo fmt --all --check
	@unformatted=$$(cd go && gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@unformatted=$$(cd examples/envoy-callout && gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed (example):"; echo "$$unformatted"; exit 1; fi

clippy:
	cargo clippy --all-targets -- -D warnings

test: fmt-check clippy cargo-test go-test examples-test verify impl impl-capi

# Manual: drives the real System One endpoint. Needs TYPESAFE_API_KEY.
# Captures every reply so it can be pinned and replayed.
live: build-go
	$(BIN) --corpus testdata/corpus --tier 1 --decider live \
	  --capture .bin/capture --out .bin/live.jsonl

# Early-warning: has upstream behaviour moved since the pin? Re-pins to main,
# re-runs the oracle against the recorded golden, and reports the diff.
drift:
	scripts/drift-check.sh drift-report.txt

clean:
	cargo clean
	rm -rf .bin
