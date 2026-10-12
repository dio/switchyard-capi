// Package harness is the conformance harness's corpus plumbing: it loads cases,
// drives the public libsygo.Router, and emits the result schema the Rust oracle
// produces. It is internal because it exists to test the library, not to be
// consumed by it.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dio/libsygo"
)

// Case is one corpus case.
type Case struct {
	CaseID  string          `json:"case_id"`
	Tier    *int            `json:"tier"`
	Profile libsygo.Profile `json:"profile"`
	Models  libsygo.Models  `json:"models"`

	// Single-turn shorthand. Ignored when Turns is present.
	Headers map[string]string `json:"headers"`
	Prompt  *string           `json:"prompt"`
	Model   *string           `json:"model"`
	Stream  *bool             `json:"stream"`
	Decider *DeciderResponses `json:"decider_responses"`

	// RawRequest is a real provider request body, decoded before routing. When set
	// it replaces Prompt, so messages_digest and instructions become the
	// normalizer's fingerprint rather than a synthetic request's.
	RawRequest json.RawMessage `json:"raw_request"`

	// WireFormat names RawRequest's format. Empty means openai_chat.
	WireFormat string `json:"wire_format"`

	// Multi-turn sequence sharing one router.
	Turns []TurnSpec `json:"turns"`
}

// TurnSpec is one turn; every field is optional.
type TurnSpec struct {
	Headers    map[string]string `json:"headers"`
	Prompt     *string           `json:"prompt"`
	Model      *string           `json:"model"`
	Stream     *bool             `json:"stream"`
	Decider    *DeciderResponses `json:"decider_responses"`
	RawRequest json.RawMessage   `json:"raw_request"`
	WireFormat string            `json:"wire_format"`
}

// DeciderResponses holds the scripted judge replies, consumed in call order.
type DeciderResponses struct {
	ByIndex []Scripted `json:"by_index"`
}

// Scripted is one scripted judge reply, in the corpus's on-disk shape.
type Scripted struct {
	Kind     string          `json:"kind"`
	Response json.RawMessage `json:"response"`
	Message  string          `json:"message"`
}

// TurnSpecs returns the turns to run: an explicit sequence, or one turn from the
// top-level shorthand.
func (c Case) TurnSpecs() []TurnSpec {
	if len(c.Turns) > 0 {
		return c.Turns
	}
	return []TurnSpec{{
		Headers:    c.Headers,
		Prompt:     c.Prompt,
		Model:      c.Model,
		Stream:     c.Stream,
		Decider:    c.Decider,
		RawRequest: c.RawRequest,
		WireFormat: c.WireFormat,
	}}
}

// Request builds the turn's normalized request, decoding a provider body when the
// case carries one.
//
// An unported wire format is an error rather than a fallback to the prompt:
// silently routing something else would let the case pass while proving nothing
// about the normalizer it exists to exercise.
func (t TurnSpec) Request() (libsygo.LlmRequest, error) {
	if len(t.RawRequest) == 0 {
		return libsygo.TextRequest(t.PromptText()), nil
	}
	switch t.WireFormat {
	case "", "openai_chat":
		return libsygo.DecodeOpenAIChat(t.RawRequest)
	case "anthropic_messages":
		return libsygo.DecodeAnthropicMessages(t.RawRequest)
	default:
		return libsygo.LlmRequest{}, fmt.Errorf("wire format %q is not ported", t.WireFormat)
	}
}

// Fixtures are the scripted replies for a turn.
func (t TurnSpec) Fixtures() []Scripted {
	if t.Decider == nil {
		return nil
	}
	return t.Decider.ByIndex
}

// Session is the turn's session key.
func (t TurnSpec) Session() string {
	return t.Headers["x-switchyard-session-id"]
}

// PromptText is the turn's prompt.
func (t TurnSpec) PromptText() string {
	if t.Prompt == nil {
		return ""
	}
	return *t.Prompt
}

// LoadDir loads every *.jsonl suite in dir, applying the same tier and case-glob
// filters the Rust harness uses.
func LoadDir(dir string, tier int, caseGlobs []string) ([]Case, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	var cases []Case
	for _, path := range entries {
		loaded, err := LoadSuite(path)
		if err != nil {
			return nil, err
		}
		for _, c := range loaded {
			if tier >= 0 && (c.Tier == nil || *c.Tier != tier) {
				continue
			}
			if !matchesAnyGlob(caseGlobs, c.CaseID) {
				continue
			}
			cases = append(cases, c)
		}
	}
	return cases, nil
}

// LoadSuite reads a JSONL suite. Blank lines and // comments are ignored.
func LoadSuite(path string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(trimmed), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// matchesAnyGlob reports whether a case id matches any of the filters. No globs
// means no filtering.
func matchesAnyGlob(globs []string, caseID string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, glob := range globs {
		if globMatch(glob, caseID) {
			return true
		}
	}
	return false
}

func globMatch(pattern, text string) bool {
	before, after, found := strings.Cut(pattern, "*")
	if !found {
		return pattern == text
	}
	return len(text) >= len(before)+len(after) &&
		strings.HasPrefix(text, before) && strings.HasSuffix(text, after)
}
