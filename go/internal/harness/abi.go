package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dio/libsygo"
	"github.com/dio/libsygo/jev"
	"github.com/dio/switchyard-capi/go/capi"
)

// Backends the runner can drive. Both consume the same corpus and emit the same
// result schema, so the Rust harness diffs either against the same golden.
const (
	// BackendPort is the pure-Go port from github.com/dio/libsygo.
	BackendPort = "port"
	// BackendCAPI drives libsy through the C ABI in go/capi.
	BackendCAPI = "capi"
)

// RunCaseCAPI runs a case against libsy through the C ABI.
//
// The ABI is libsy itself, so this is the reference: where it and the port
// disagree, the port is wrong. Its point is to make that comparison mechanical.
func RunCaseCAPI(ctx context.Context, c Case, options Options) ([]TurnResult, error) {
	kind, configJSON, err := abiAlgorithm(c.Profile)
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", c.CaseID, err)
	}
	algorithm, err := capi.New(kind, configJSON)
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", c.CaseID, err)
	}
	defer algorithm.Close()

	modelsJSON, err := json.Marshal(map[string][]string(c.Models))
	if err != nil {
		return nil, fmt.Errorf("case %s: encoding the model pool: %w", c.CaseID, err)
	}

	turns := c.TurnSpecs()
	results := make([]TurnResult, 0, len(turns))
	for index, turn := range turns {
		result, err := runTurnCAPI(ctx, c.CaseID, index, turn, algorithm, string(modelsJSON), options)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func runTurnCAPI(
	ctx context.Context,
	caseID string,
	index int,
	turn TurnSpec,
	algorithm *capi.Algorithm,
	modelsJSON string,
	options Options,
) (TurnResult, error) {
	request, err := turn.Request()
	if err != nil {
		// The port's decoder refuses this request, and so does the oracle's: report
		// the same rejection rather than letting this backend route an input the
		// other one rejected.
		return RejectedResult(caseID, index, err.Error()), nil
	}
	requestJSON, err := abiRequest(request, turn.Headers)
	if err != nil {
		return TurnResult{}, fmt.Errorf("case %s turn %d: %w", caseID, index, err)
	}

	run, err := algorithm.Start(requestJSON, modelsJSON, "")
	if err != nil {
		return TurnResult{}, fmt.Errorf("case %s turn %d: %w", caseID, index, err)
	}
	defer run.Close()

	fixtures := turn.Fixtures()
	consumed, calls := 0, 0

	var outcome *capi.Outcome
	var failure string

	for {
		step, err := run.Next()
		if err != nil {
			return TurnResult{}, fmt.Errorf("case %s turn %d: %w", caseID, index, err)
		}

		switch step.Kind {
		case capi.StepCallDecision:
			calls++
			reply, err := serveDecisionCAPI(ctx, step, fixtures, &consumed, options)
			if err != nil {
				_ = run.Fail(step.ID, err.Error())
				continue
			}
			if err := run.Respond(step.ID, reply); err != nil {
				return TurnResult{}, fmt.Errorf("case %s turn %d: %w", caseID, index, err)
			}

		case capi.StepCallModel:
			// The tier-0/1 corpus never offloads a model call; if one appears the
			// case is outside what this backend serves, so say so rather than
			// guessing.
			calls++
			_ = run.Fail(step.ID, "the ABI backend does not serve model calls")

		case capi.StepDone:
			outcome = step.Outcome

		case capi.StepError:
			failure = step.Message

		case capi.StepEnd:
			// The stream ended without a terminal step.
		}

		if outcome != nil || failure != "" || step.Kind == capi.StepEnd {
			break
		}
	}

	return toResultFromABI(caseID, index, turn.PromptText(), outcome, failure, calls, len(fixtures)-consumed)
}

// serveDecisionCAPI answers one judge call: from a live decider when configured,
// otherwise from the turn's scripted reply.
func serveDecisionCAPI(
	ctx context.Context,
	step capi.Step,
	fixtures []Scripted,
	consumed *int,
	options Options,
) (string, error) {
	if options.Judged != nil {
		var request jev.DecisionRequest
		if err := json.Unmarshal(step.Request, &request); err != nil {
			return "", fmt.Errorf("decoding the judge request: %w", err)
		}
		response, err := options.Judged.Call(ctx, request)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}

	if *consumed >= len(fixtures) {
		return "", errors.New("unexpected judge call: the case scripted no reply")
	}
	reply := fixtures[*consumed]
	*consumed++

	switch reply.Kind {
	case "decision":
		return string(reply.Response), nil
	case "decision_error":
		return "", errors.New(reply.Message)
	default:
		return "", fmt.Errorf("scripted step %q cannot serve a judge call", reply.Kind)
	}
}

// toResultFromABI maps an ABI run onto the oracle's result schema.
func toResultFromABI(
	caseID string,
	index int,
	prompt string,
	outcome *capi.Outcome,
	failure string,
	calls int,
	unconsumed int,
) (TurnResult, error) {
	if failure != "" {
		steps := []string{"Error"}
		if calls > 0 {
			steps = []string{"CallDecision", "Error"}
		}
		return ErrorResult(caseID, index, steps, unconsumed, failure), nil
	}
	if outcome == nil {
		return TurnResult{}, fmt.Errorf("case %s turn %d: the run produced no outcome", caseID, index)
	}

	// The rewritten request comes back as libsy serialized it, so its `messages`
	// bytes are hashed verbatim: re-encoding through Go would reorder object keys
	// and change the digest.
	var rewritten struct {
		Instructions json.RawMessage `json:"instructions"`
		Messages     json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(outcome.Request, &rewritten); err != nil {
		return TurnResult{}, fmt.Errorf("decoding the rewritten request: %w", err)
	}

	instructions := rewritten.Instructions
	if len(instructions) == 0 {
		instructions = json.RawMessage(`[]`)
	}

	var metadata struct {
		Algorithm string          `json:"algorithm"`
		Evidence  json.RawMessage `json:"evidence"`
	}
	if len(outcome.Metadata) > 0 {
		if err := json.Unmarshal(outcome.Metadata, &metadata); err != nil {
			return TurnResult{}, fmt.Errorf("decoding the outcome metadata: %w", err)
		}
	}

	steps := []string{"Done"}
	if calls > 0 {
		steps = []string{"CallDecision", "Done"}
	}

	selected := outcome.SelectedModelIDs
	var head *string
	if len(selected) > 0 {
		head = strptr(selected[0])
	}
	var algorithm = &metadata.Algorithm
	if metadata.Algorithm == "" {
		algorithm = nil
	}

	return TurnResult{
		CaseID:             caseID,
		Turn:               index,
		Steps:              steps,
		StepCount:          len(steps),
		SelectedModelIDs:   selected,
		SelectedModelID:    head,
		RewrittenModel:     head,
		Instructions:       instructions,
		MessagesDigest:     digestRaw(rewritten.Messages),
		Evidence:           metadata.Evidence,
		Algorithm:          algorithm,
		UnconsumedScripted: unconsumed,
		ResponsePresent:    outcome.ResponsePresent,
	}, nil
}

// abiAlgorithm translates a corpus profile into the ABI's kind and config JSON.
//
// The corpus profile is the neutral, algorithm-agnostic shape; the ABI wants the
// algorithm's own config. Only the decision judge is implemented, so anything
// else is refused rather than approximated.
func abiAlgorithm(p libsygo.Profile) (kind, configJSON string, err error) {
	if p.Algorithm != "llm_classifier" {
		return "", "", fmt.Errorf("the ABI backend implements llm_classifier only, not %q", p.Algorithm)
	}
	if p.Mode != "capability" {
		return "", "", fmt.Errorf("the ABI backend implements capability only, not %q", p.Mode)
	}
	if p.Judge == nil || p.Judge.Type != "decision" {
		return "", "", errors.New("the ABI backend implements the decision judge only")
	}

	failOpen := true
	if p.FailOpen != nil {
		failOpen = *p.FailOpen
	}
	trigger := "every_request"
	if p.SessionAffinity {
		trigger = "new_session"
	}

	config, err := json.Marshal(map[string]any{
		"cutoff":           p.Judge.Cutoff,
		"candidates":       p.Judge.Candidates,
		"evidence":         p.Judge.Evidence,
		"instructions":     p.Judge.Instructions,
		"fail_open":        failOpen,
		"classify_trigger": trigger,
	})
	if err != nil {
		return "", "", err
	}
	return "llm_classifier", string(config), nil
}

// abiRequest builds the ABI's request envelope, from the same decoded request the
// port routes. Building it from a prompt instead would compare the two backends on
// different inputs, and the ABI result would then say nothing about the normalizer.
func abiRequest(request libsygo.LlmRequest, headers map[string]string) (string, error) {
	if request.Model == "" {
		request.Model = "auto"
	}

	envelope := struct {
		LlmRequest json.RawMessage   `json:"llm_request"`
		Headers    map[string]string `json:"headers"`
	}{
		LlmRequest: libsygo.LlmRequestJSON(request),
		Headers:    headers,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
