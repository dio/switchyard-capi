BIN := .bin/libsy-conform-impl

.PHONY: all build build-rust build-go verify impl fmt-check clippy test clean

all: test

build: build-rust build-go

build-rust:
	cargo build

build-go:
	mkdir -p .bin
	cd go && go build -o ../$(BIN) ./cmd/libsy-conform-impl

# Oracle vs recorded golden: proves the oracle is deterministic and upstream
# behaviour has not moved for the pinned revision.
verify:
	cargo run -q -p libsy-conform -- --mode verify

# The tier-0/1 Go port vs golden. Parallel-safe with `verify`; the port covers
# llm_classifier + the decision judge only, so it is tier-filtered.
impl: build-go
	cargo run -q -p libsy-conform -- --mode impl --tier 1

fmt-check:
	cargo fmt --all --check
	@unformatted=$$(cd go && gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

clippy:
	cargo clippy --all-targets -- -D warnings

test: fmt-check clippy verify impl

clean:
	cargo clean
	rm -rf .bin
