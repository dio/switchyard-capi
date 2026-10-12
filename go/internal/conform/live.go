package conform

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dio/switchyard-capi/go/internal/jev"
)

// Aliases so the port reads against the wire package's IR types directly.
type (
	DecisionRequest  = jev.DecisionRequest
	DecisionResponse = jev.DecisionResponse
)

// relativeAdvantagePrompt is the packaged capability-judge instructions, copied
// from crates/libsy/src/prompts/capability-classifier/relative_advantage.json. It
// is data, so it drifts with upstream; the copy is byte-identical at the pin and
// its checksum is recorded in PROVENANCE.md.
//
//go:embed prompts/relative_advantage.json
var relativeAdvantagePrompt []byte

// Capability-judge answer descriptions, from DecisionClassifier::new.
const (
	advantageDescription   = "The capable candidate succeeds AND the efficient candidate fails."
	noAdvantageDescription = "The efficient candidate succeeds OR the capable candidate fails, including shared failure."
)

// LiveDeciderFromEnv builds a JEV client from TYPESAFE_API_KEY, optionally
// overriding TYPESAFE_ENDPOINT and TYPESAFE_TIMEOUT_MS.
func LiveDeciderFromEnv() (Decider, error) {
	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		return nil, errors.New("TYPESAFE_API_KEY is required for a live decider")
	}
	endpoint := os.Getenv("TYPESAFE_ENDPOINT")
	if endpoint == "" {
		endpoint = jev.DefaultEndpoint
	}
	timeout := 10 * time.Second
	if raw := os.Getenv("TYPESAFE_TIMEOUT_MS"); raw != "" {
		millis, err := strconv.Atoi(raw)
		if err != nil || millis <= 0 {
			return nil, fmt.Errorf("TYPESAFE_TIMEOUT_MS must be a positive integer, got %q", raw)
		}
		timeout = time.Duration(millis) * time.Millisecond
	}
	return jev.NewClient(endpoint, apiKey, timeout)
}

// NewCapture writes each live reply into dir as `<case>.<turn>.json`, in the
// same scripted-reply shape the corpus uses, so a capture can be pasted straight
// into `decider_responses.by_index`.
func NewCapture(dir string) (captureFunc, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return func(caseID string, turn int, reply Scripted) error {
		path := filepath.Join(dir, fmt.Sprintf("%s.%d.json", sanitize(caseID), turn))
		body, err := json.MarshalIndent(reply, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(body, '\n'), 0o644)
	}, nil
}

func sanitize(caseID string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(caseID)
}

// buildDecisionRequest assembles the context and question that
// DecisionClassifier sends to the judge.
func (a *Algorithm) buildDecisionRequest(prompt string) (DecisionRequest, error) {
	comparison, err := a.comparison()
	if err != nil {
		return DecisionRequest{}, err
	}

	context, err := json.Marshal(map[string]any{
		"task": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": prompt}},
		}},
		"candidates": a.candidateLabels(),
		"comparison": comparison,
		"evidence":   a.evidence,
	})
	if err != nil {
		return DecisionRequest{}, err
	}

	options, err := json.Marshal(map[string]any{
		"options": []map[string]any{
			{"id": "advantage", "description": advantageDescription},
			{"id": "no_advantage", "description": noAdvantageDescription},
		},
	})
	if err != nil {
		return DecisionRequest{}, err
	}

	model := a.judgeModel
	return DecisionRequest{
		Model:   &model,
		Context: context,
		Questions: map[string]jev.DecisionQuestion{
			"route": {Instructions: a.instructions, Kind: jev.Tagged{Type: "choice", Data: options}},
		},
	}, nil
}

// candidateLabels are the labels in the order libsy's BTreeMap yields them.
func (a *Algorithm) candidateLabels() []string {
	labels := make([]string, 0, len(a.candidates))
	for label := range a.candidates {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// comparison names the labels of the runtime capable and efficient targets.
func (a *Algorithm) comparison() (map[string]string, error) {
	capable, err := a.labelFor(a.first("capable"))
	if err != nil {
		return nil, err
	}
	efficient, err := a.labelFor(a.first("efficient"))
	if err != nil {
		return nil, err
	}
	return map[string]string{"capable": capable, "efficient": efficient}, nil
}

func (a *Algorithm) labelFor(model string) (string, error) {
	for _, label := range a.candidateLabels() {
		if a.candidates[label] == model {
			return label, nil
		}
	}
	return "", fmt.Errorf("decision candidate is missing for target %q", model)
}
