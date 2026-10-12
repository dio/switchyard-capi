// Package conform is a tier-0/1 port scaffold: the Switchyard decision core that
// owns nothing but the judge plumbing. Tier 0 is the JEV/System One wire
// protocol; tier 1 is the cascade, the session latch, the cutoff, fail-open, and
// candidate ordering.
//
// Tier 3 (stage_router / tool signals) is deliberately absent — that is the
// module where porting is most expensive, most churn-prone, and least verifiable,
// and the recommendation is to bind it rather than port it.
package conform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Case is one corpus case.
type Case struct {
	CaseID   string              `json:"case_id"`
	Tier     *int                `json:"tier"`
	Profile  Profile             `json:"profile"`
	Models   map[string][]string `json:"models"`
	Subagent map[string][]string `json:"subagent_models"`
	Headers  map[string]string   `json:"headers"`
	Prompt   *string             `json:"prompt"`
	Model    *string             `json:"model"`
	Stream   *bool               `json:"stream"`
	Decider  *DeciderResponses   `json:"decider_responses"`
	Turns    []TurnSpec          `json:"turns"`
}

// Profile is the tenant-selectable profile: algorithm plus its config.
type Profile struct {
	Algorithm           string     `json:"algorithm"`
	Mode                string     `json:"mode"`
	Judge               *JudgeSpec `json:"judge"`
	FailOpen            *bool      `json:"fail_open"`
	SessionAffinity     bool       `json:"session_affinity"`
	MessageHashFallback bool       `json:"message_hash_fallback"`
	RecentTurnWindow    *int       `json:"recent_turn_window"`
}

// JudgeSpec is the judge configuration. Only the decision judge is ported.
type JudgeSpec struct {
	Type         string            `json:"type"`
	Cutoff       float64           `json:"cutoff"`
	Candidates   map[string]string `json:"candidates"`
	Evidence     json.RawMessage   `json:"evidence"`
	Instructions json.RawMessage   `json:"instructions"`
}

// TurnSpec is one turn; every field is optional.
type TurnSpec struct {
	Headers map[string]string `json:"headers"`
	Prompt  *string           `json:"prompt"`
	Model   *string           `json:"model"`
	Stream  *bool             `json:"stream"`
	Decider *DeciderResponses `json:"decider_responses"`
}

// DeciderResponses holds the scripted replies, consumed in step order.
type DeciderResponses struct {
	ByIndex []Scripted `json:"by_index"`
}

// Scripted is one scripted reply.
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
		Headers: c.Headers,
		Prompt:  c.Prompt,
		Model:   c.Model,
		Stream:  c.Stream,
		Decider: c.Decider,
	}}
}

// LoadDir loads every *.jsonl suite in dir, applying the same tier and case-glob
// filters the Rust harness uses.
func LoadDir(dir string, tier int, caseGlob string) ([]Case, error) {
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
			if caseGlob != "" && !globMatch(caseGlob, c.CaseID) {
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

func globMatch(pattern, text string) bool {
	before, after, found := strings.Cut(pattern, "*")
	if !found {
		return pattern == text
	}
	return len(text) >= len(before)+len(after) &&
		strings.HasPrefix(text, before) && strings.HasSuffix(text, after)
}
