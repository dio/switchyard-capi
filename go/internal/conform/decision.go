package conform

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// algorithmName matches libsy's `Algorithm::name` for the capability classifier.
const algorithmName = "llm_task_classifier"

// defaultFallbackCategory is where an abstaining capability classifier lands: the
// cascade closes with a default classifier for the capable category.
const defaultFallbackCategory = "capable"

// Decider is one decision call. jev.Client satisfies it; tests substitute a fake.
type Decider interface {
	Call(ctx context.Context, request DecisionRequest) (DecisionResponse, error)
}

// captureFunc records a live decider reply, in the corpus's scripted reply shape,
// so it can be pinned and replayed deterministically.
type captureFunc func(caseID string, turn int, reply Scripted) error

// Algorithm is the tier-1 port: a two-stage cascade over a decision judge, plus
// the session latch.
//
// Cascade order matters and is reproduced from libsy: the affinity classifier is
// consulted first, so a latched session reuses its target and never reaches the
// judge; when the judge abstains (fail-open), a default classifier closes the
// cascade on the capable tier.
type Algorithm struct {
	cutoff          float64
	failOpen        bool
	sessionAffinity bool
	models          map[string][]string

	// Judge inputs, needed only when a live decider is attached — replay mode
	// consumes the neutral IR from the corpus instead.
	candidates   map[string]string
	judgeModel   string
	instructions json.RawMessage
	evidence     json.RawMessage

	decider Decider
	capture captureFunc

	latched map[string]latch
}

type latch struct {
	model    string
	category string
}

// Options configures an optional live decider and capture sink.
type Options struct {
	Decider Decider
	Capture captureFunc
}

// NewAlgorithm builds the port for a profile, rejecting anything outside tier 0/1
// rather than guessing.
func NewAlgorithm(profile Profile, models map[string][]string, options Options) (*Algorithm, error) {
	if profile.Algorithm != "llm_classifier" {
		return nil, fmt.Errorf(
			"profile algorithm %q is not ported: tier 0/1 covers llm_classifier only",
			profile.Algorithm)
	}
	if profile.Mode != "capability" {
		return nil, fmt.Errorf(
			"llm_classifier mode %q is not ported: tier 0/1 covers capability only", profile.Mode)
	}
	if profile.Judge == nil || profile.Judge.Type != "decision" {
		return nil, fmt.Errorf(
			"judge is not ported: tier 0/1 covers the decision (JEV / System One) judge only")
	}
	failOpen := true
	if profile.FailOpen != nil {
		failOpen = *profile.FailOpen
	}

	instructions := profile.Judge.Instructions
	if len(instructions) == 0 {
		instructions = json.RawMessage(relativeAdvantagePrompt)
	}

	algorithm := &Algorithm{
		cutoff:          profile.Judge.Cutoff,
		failOpen:        failOpen,
		sessionAffinity: profile.SessionAffinity,
		models:          models,
		candidates:      profile.Judge.Candidates,
		instructions:    instructions,
		evidence:        profile.Judge.Evidence,
		decider:         options.Decider,
		capture:         options.Capture,
		latched:         map[string]latch{},
	}

	if algorithm.decider != nil {
		judgeModels := models["judge"]
		if len(judgeModels) == 0 {
			return nil, fmt.Errorf("profile has no judge model, required by the live decider")
		}
		algorithm.judgeModel = judgeModels[0]
	}
	return algorithm, nil
}

// RunCase runs every turn of a case against one algorithm, so the latch persists
// across turns exactly as it would in production.
func RunCase(ctx context.Context, c Case, options Options) ([]TurnResult, error) {
	algorithm, err := NewAlgorithm(c.Profile, c.Models, options)
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", c.CaseID, err)
	}
	turns := c.TurnSpecs()
	results := make([]TurnResult, 0, len(turns))
	for index, turn := range turns {
		results = append(results, algorithm.runTurn(ctx, c.CaseID, index, turn))
	}
	return results, nil
}

func (a *Algorithm) runTurn(ctx context.Context, caseID string, index int, turn TurnSpec) TurnResult {
	prompt := ""
	if turn.Prompt != nil {
		prompt = *turn.Prompt
	}
	session := turn.Headers["x-switchyard-session-id"]

	var scripted []Scripted
	if turn.Decider != nil {
		scripted = turn.Decider.ByIndex
	}

	// Affinity leads the cascade: a latched session decides without a judge call.
	if a.sessionAffinity && session != "" {
		if held, ok := a.latched[session]; ok {
			return a.outcome(caseID, index, prompt, []string{"Done"}, held.model, held.category,
				json.RawMessage(`{"source":"retained"}`), len(scripted))
		}
	}

	// One decision path, whichever way the judge reply arrives.
	var reply Scripted
	unconsumed := 0
	if a.decider != nil {
		live, err := a.callDecider(ctx, caseID, index, prompt)
		if err != nil {
			return a.failOpenOrNot(caseID, index, prompt, len(scripted), "call_error", err.Error())
		}
		reply, unconsumed = live, len(scripted)
	} else {
		if len(scripted) == 0 {
			return ErrorResult(caseID, index, []string{"CallDecision", "Error"}, 0,
				"unexpected CallDecision: the case scripted no response")
		}
		reply, unconsumed = scripted[0], len(scripted)-1
	}

	switch reply.Kind {
	case "decision":
		score, ok := advantageScore(reply.Response)
		if !ok {
			return a.failOpenOrNot(caseID, index, prompt, unconsumed, "invalid_verdict", "")
		}
		model, category := a.tierFor(score)
		evidence, _ := json.Marshal(map[string]any{
			"source":    "decision_classifier",
			"verdict":   "relative_advantage",
			"score":     score,
			"threshold": a.cutoff,
		})
		if a.sessionAffinity && session != "" {
			a.latched[session] = latch{model: model, category: category}
		}
		return a.outcome(caseID, index, prompt, []string{"CallDecision", "Done"}, model, category, evidence, unconsumed)

	case "decision_error":
		return a.failOpenOrNot(caseID, index, prompt, unconsumed, "call_error", reply.Message)

	default:
		return ErrorResult(caseID, index, []string{"CallDecision", "Error"}, unconsumed,
			fmt.Sprintf("scripted step %q cannot serve a decision call", reply.Kind))
	}
}

// callDecider performs a live decision call and, when a sink is configured,
// records the reply so it can be replayed.
func (a *Algorithm) callDecider(ctx context.Context, caseID string, index int, prompt string) (Scripted, error) {
	request, err := a.buildDecisionRequest(prompt)
	if err != nil {
		return Scripted{}, err
	}
	response, err := a.decider.Call(ctx, request)
	if err != nil {
		return Scripted{}, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return Scripted{}, err
	}
	reply := Scripted{Kind: "decision", Response: encoded}
	if a.capture != nil {
		_ = a.capture(caseID, index, reply)
	}
	return reply, nil
}

// failOpenOrNot handles a judge that produced no usable verdict: fail-open lands
// on the default capable tier, fail-closed propagates the judge error.
func (a *Algorithm) failOpenOrNot(caseID string, index int, prompt string, unconsumed int, reason, judgeMessage string) TurnResult {
	if !a.failOpen {
		return ErrorResult(caseID, index, []string{"CallDecision", "Error"}, unconsumed, judgeMessage)
	}
	evidence, _ := json.Marshal(map[string]any{"source": "fail_open", "reason_code": reason})
	return a.outcome(caseID, index, prompt, []string{"CallDecision", "Done"},
		a.first(defaultFallbackCategory), defaultFallbackCategory, evidence, unconsumed)
}

func (a *Algorithm) tierFor(advantage float64) (model, category string) {
	// Strictly greater: at the cutoff the capable tier is not selected.
	if advantage > a.cutoff {
		return a.first("capable"), "capable"
	}
	return a.first("efficient"), "efficient"
}

func (a *Algorithm) first(category string) string {
	if list := a.models[category]; len(list) > 0 {
		return list[0]
	}
	return ""
}

// ordered reproduces libsy's fallback ordering: the pick, then the rest of the
// category it was drawn from, then every other runtime target.
func (a *Algorithm) ordered(selected, category string) []string {
	out := []string{selected}
	add := func(list []string) {
		for _, model := range list {
			if model == selected || slices.Contains(out, model) {
				continue
			}
			out = append(out, model)
		}
	}
	add(a.models[category])
	add(a.models["any"])
	return out
}

func (a *Algorithm) outcome(caseID string, index int, prompt string, steps []string,
	model, category string, evidence json.RawMessage, unconsumed int) TurnResult {
	return TurnResult{
		CaseID:             caseID,
		Turn:               index,
		Steps:              steps,
		StepCount:          len(steps),
		SelectedModelIDs:   a.ordered(model, category),
		SelectedModelID:    strptr(model),
		RewrittenModel:     strptr(model),
		Instructions:       json.RawMessage(`[]`),
		MessagesDigest:     MessagesDigest(prompt),
		Evidence:           evidence,
		Algorithm:          strptr(algorithmName),
		UnconsumedScripted: unconsumed,
	}
}

// advantageScore reads p(advantage) from the neutral decision IR, mirroring
// DecisionClassifier: only a Choice answer with a probability in [0,1] counts.
func advantageScore(raw json.RawMessage) (float64, bool) {
	var response struct {
		Answers map[string]struct {
			Value struct {
				Type string `json:"type"`
				Data struct {
					Probabilities map[string]float64 `json:"probabilities"`
				} `json:"data"`
			} `json:"value"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return 0, false
	}
	answer, ok := response.Answers["route"]
	if !ok || answer.Value.Type != "choice" {
		return 0, false
	}
	score, ok := answer.Value.Data.Probabilities["advantage"]
	if !ok || score < 0 || score > 1 {
		return 0, false
	}
	return score, true
}
