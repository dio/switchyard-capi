package capi

import (
	"encoding/json"
	"testing"
)

const autoRequest = `{"llm_request":{"model":"auto","messages":[
  {"role":"user","content":[{"type":"text","text":"refactor the payment module"}]}]}}`

const capabilityModels = `{"capable":["smart"],"efficient":["cheap"],"judge":["jev-latest"],"any":["cheap","smart"]}`

const capabilityConfig = `{"cutoff":0.4,"candidates":{"a":"smart","b":"cheap"},
  "evidence":{"candidates":{"a":"capable tier","b":"efficient tier"}}}`

func TestVersionIsPresent(t *testing.T) {
	if Version() == "" {
		t.Fatal("the ABI reported no version")
	}
}

// The DTO-free path: construction, start, and a run that completes with no host
// call at all.
func TestNoopCompletesWithoutCalls(t *testing.T) {
	algorithm, err := New("noop", "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer algorithm.Close()

	run, err := algorithm.Start(autoRequest, `{"any":["m/any"]}`, "")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	step, err := run.Next()
	if err != nil {
		t.Fatal(err)
	}
	if step.Kind != StepDone {
		t.Fatalf("step kind = %q, want %q", step.Kind, StepDone)
	}
	if step.Outcome == nil || len(step.Outcome.SelectedModelIDs) == 0 {
		t.Fatal("done carried no outcome")
	}
	if got := step.Outcome.SelectedModelIDs[0]; got != "auto" {
		t.Fatalf("selected = %q, want auto", got)
	}
}

// The DTO path over the real pull loop: the judge call comes back as a step, the
// host answers it, and the run routes on the reply.
func TestDecisionJudgePullLoop(t *testing.T) {
	algorithm, err := New("llm_classifier", capabilityConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer algorithm.Close()

	run, err := algorithm.Start(autoRequest, capabilityModels, "")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	step, err := run.Next()
	if err != nil {
		t.Fatal(err)
	}
	if step.Kind != StepCallDecision {
		t.Fatalf("step kind = %q, want %q", step.Kind, StepCallDecision)
	}
	if step.Model != "jev-latest" {
		t.Fatalf("judge model = %q", step.Model)
	}
	// The neutral IR nests its tagged enum, unlike System One's flat wire shape.
	var question struct {
		Questions map[string]struct {
			Kind struct {
				Type string `json:"type"`
			} `json:"kind"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(step.Request, &question); err != nil {
		t.Fatal(err)
	}
	if got := question.Questions["route"].Kind.Type; got != "choice" {
		t.Fatalf("question kind = %q, want choice", got)
	}

	verdict := `{"answers":{"route":{"value":{"type":"choice","data":{
      "selected":"advantage","probabilities":{"advantage":0.77,"no_advantage":0.23}}}}}}`
	if err := run.Respond(step.ID, verdict); err != nil {
		t.Fatal(err)
	}

	done, err := run.Next()
	if err != nil {
		t.Fatal(err)
	}
	if done.Kind != StepDone {
		t.Fatalf("step kind = %q, want %q", done.Kind, StepDone)
	}
	if got := done.Outcome.SelectedModelIDs[0]; got != "smart" {
		t.Fatalf("selected = %q, want smart (0.77 is above the 0.4 cutoff)", got)
	}

	var metadata struct {
		Algorithm string `json:"algorithm"`
		Evidence  struct {
			Score     float64 `json:"score"`
			Threshold float64 `json:"threshold"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(done.Outcome.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Evidence.Score != 0.77 || metadata.Evidence.Threshold != 0.4 {
		t.Fatalf("evidence = %+v", metadata)
	}
}

// A host failure is handed back so the algorithm applies its fallback policy
// rather than the run dying at the boundary.
func TestFailedCallFallsOpen(t *testing.T) {
	algorithm, err := New("llm_classifier", capabilityConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer algorithm.Close()

	run, err := algorithm.Start(autoRequest, capabilityModels, "")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	step, err := run.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fail(step.ID, "judge unavailable"); err != nil {
		t.Fatal(err)
	}

	done, err := run.Next()
	if err != nil {
		t.Fatal(err)
	}
	if done.Kind != StepDone {
		t.Fatalf("step kind = %q, want %q", done.Kind, StepDone)
	}
	if got := done.Outcome.SelectedModelIDs[0]; got != "smart" {
		t.Fatalf("selected = %q; fail-open defaults to the capable tier", got)
	}

	var evidence struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(done.Outcome.Metadata, &evidence); err != nil {
		t.Fatal(err)
	}
	_ = evidence
}

// A malformed request is refused at the boundary, not mid-route.
func TestBadInputIsRefused(t *testing.T) {
	if _, err := New("not-an-algorithm", "{}"); err == nil {
		t.Fatal("an unknown kind must fail")
	}

	algorithm, err := New("llm_classifier", capabilityConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer algorithm.Close()

	if _, err := algorithm.Start(autoRequest, `{"capable":["smart"]}`, ""); err == nil {
		t.Fatal("a pool without `any` must be refused")
	}
}
