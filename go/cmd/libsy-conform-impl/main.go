// Command libsy-conform-impl is the tier-0/1 Go port runner.
//
// It consumes the same corpus as the Rust oracle and emits the same result
// schema, so `libsy-conform --mode impl` can diff the two. Anything outside
// tier 0/1 is rejected with a clear error rather than approximated.
//
//	go build -o .bin/libsy-conform-impl ./go/cmd/libsy-conform-impl
//	libsy-conform --mode impl --tier 1
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/dio/switchyard-capi/go/internal/conform"
)

func main() {
	corpus := flag.String("corpus", "testdata/corpus", "corpus directory of *.jsonl suites")
	out := flag.String("out", "", "results JSONL path (default stdout)")
	tier := flag.Int("tier", -1, "only cases with this tier")
	caseGlob := flag.String("case", "", "only case_ids matching this glob")
	flag.Parse()

	cases, err := conform.LoadDir(*corpus, *tier, *caseGlob)
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

	written := 0
	for _, c := range cases {
		results, err := conform.RunCase(c)
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
	fmt.Fprintf(os.Stderr, "libsy-conform-impl: %d turn(s) from %d case(s)\n", written, len(cases))
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "libsy-conform-impl: %v\n", err)
	os.Exit(1)
}
