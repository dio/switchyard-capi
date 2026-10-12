// Command libsy-conform-impl is the tier-0/1 Go port runner.
//
// It consumes the same corpus as the Rust oracle and emits the same result
// schema, so `libsy-conform --mode impl` can diff the two. Anything outside
// tier 0/1 is rejected with a clear error rather than approximated.
//
//	go build -o .bin/libsy-conform-impl ./go/cmd/libsy-conform-impl
//	libsy-conform --mode impl --tier 1
//
// With TYPESAFE_API_KEY set, --decider live calls the real System One endpoint
// instead of replaying fixtures, and --capture records the replies so they can be
// pinned and replayed deterministically.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/dio/switchyard-capi/go/internal/harness"
)

func main() {
	corpus := flag.String("corpus", "testdata/corpus", "corpus directory of *.jsonl suites")
	out := flag.String("out", "", "results JSONL path (default stdout)")
	tier := flag.Int("tier", -1, "only cases with this tier")
	caseGlob := flag.String("case", "", "only case_ids matching this glob")
	decider := flag.String("decider", "replay", "replay | live (live needs TYPESAFE_API_KEY)")
	capture := flag.String("capture", "", "directory to record live replies into")
	backend := flag.String("backend", backendFromEnv(),
		"port | capi (port is the pure-Go port; capi drives libsy through the C ABI)")
	flag.Parse()

	options := harness.Options{Backend: *backend, CaptureDir: *capture}
	switch *decider {
	case "replay":
		// Deterministic default: consume each turn's scripted replies.
	case "live":
		live, err := harness.NewLiveDecider()
		if err != nil {
			fail(err)
		}
		options.Judged = live
	default:
		fail(fmt.Errorf("unknown --decider %q (replay|live)", *decider))
	}

	cases, err := harness.LoadDir(*corpus, *tier, *caseGlob)
	if err != nil {
		fail(err)
	}
	if len(cases) == 0 {
		fail(fmt.Errorf("no corpus cases found in %s", *corpus))
	}

	writer := os.Stdout
	if *out != "" {
		file, err := os.Create(*out)
		if err != nil {
			fail(err)
		}
		defer file.Close()
		writer = file
	}
	buffered := bufio.NewWriter(writer)
	encoder := json.NewEncoder(buffered)

	ctx := context.Background()
	written := 0
	for _, c := range cases {
		results, err := harness.RunCase(ctx, c, options)
		if err != nil {
			fail(err)
		}
		for _, result := range results {
			if err := encoder.Encode(result); err != nil {
				fail(err)
			}
			written++
		}
	}
	if err := buffered.Flush(); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "libsy-conform-impl: %d turn(s) from %d case(s) (backend=%s decider=%s)\n",
		written, len(cases), *backend, *decider)
}

// backendFromEnv lets a caller select the backend without changing how the Rust
// harness invokes this binary: the harness passes corpus/out/tier/case, and the
// environment carries the rest.
func backendFromEnv() string {
	if backend := os.Getenv("LIBSY_CONFORM_BACKEND"); backend != "" {
		return backend
	}
	return harness.BackendPort
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "libsy-conform-impl: %v\n", err)
	os.Exit(1)
}
