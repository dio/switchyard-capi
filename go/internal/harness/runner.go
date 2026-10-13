package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dio/libsygo"
	"github.com/dio/libsygo/jev"
)

// Options configures a corpus run.
type Options struct {
	// Backend selects which implementation is driven. Empty means BackendPort.
	Backend string

	// Judged supplies judge calls. Nil replays each turn's scripted replies,
	// which is the deterministic default. Pass a jev.Client to judge live.
	Judged libsygo.Decider

	// CaptureDir records every live reply, in the corpus's scripted-reply shape,
	// so it can be pinned and replayed.
	CaptureDir string
}

// RunCase runs a case against the selected backend. Both backends consume the
// same corpus and emit the same schema, so the harness diffs either against the
// same golden.
func RunCase(ctx context.Context, c Case, options Options) ([]TurnResult, error) {
	switch options.Backend {
	case "", BackendPort:
		return runCasePort(ctx, c, options)
	case BackendCAPI:
		return RunCaseCAPI(ctx, c, options)
	default:
		return nil, fmt.Errorf("unknown backend %q (want %q or %q)",
			options.Backend, BackendPort, BackendCAPI)
	}
}

// runCasePort runs a case against the pure-Go port.
func runCasePort(ctx context.Context, c Case, options Options) ([]TurnResult, error) {
	capture, err := captureSink(options.CaptureDir)
	if err != nil {
		return nil, err
	}

	decider := &turnDecider{live: options.Judged, capture: capture}
	router, err := libsygo.New(c.Profile, c.Models)
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", c.CaseID, err)
	}

	turns := c.TurnSpecs()
	results := make([]TurnResult, 0, len(turns))
	for index, turn := range turns {
		request, err := turn.Request()
		if err != nil {
			// A request the port will not decode is a rejection, not a harness
			// failure: the oracle records the same label for input libsy refuses,
			// so "rejected rather than coerced" is the behaviour being compared.
			results = append(results, RejectedResult(c.CaseID, index, err.Error()))
			continue
		}
		decider.begin(c.CaseID, index, turn.Fixtures())
		outcome := router.Route(ctx, libsygo.Turn{
			Request:  request,
			Session:  turn.Session(),
			Metadata: libsygo.MetadataFromHeaders(turn.Headers),
		}, decider)
		results = append(results, toResult(c.CaseID, index, outcome, decider))
	}
	return results, nil
}

// turnDecider is the bridge between the router and the corpus: it serves judge
// calls from the scripted replies, or delegates to a live host decider, and
// records what it consumed.
type turnDecider struct {
	live     libsygo.Decider
	capture  captureFunc
	fixtures []Scripted
	consumed int
	calls    int
	// judgeTask is what the first judge call of the turn showed, which is the turn's
	// selection rather than the conversation.
	judgeTask json.RawMessage
	caseID    string
	turn      int
}

func (d *turnDecider) begin(caseID string, turn int, fixtures []Scripted) {
	d.caseID = caseID
	d.turn = turn
	d.fixtures = fixtures
	d.consumed = 0
	d.calls = 0
	d.judgeTask = nil
}

func (d *turnDecider) unconsumed() int { return len(d.fixtures) - d.consumed }

func (d *turnDecider) Call(ctx context.Context, request jev.DecisionRequest) (jev.DecisionResponse, error) {
	d.calls++
	if d.judgeTask == nil {
		d.judgeTask = judgeTaskOf(request)
	}

	if d.live != nil {
		response, err := d.live.Call(ctx, request)
		if err != nil {
			return jev.DecisionResponse{}, err
		}
		if d.capture != nil {
			if encoded, err := json.Marshal(response); err == nil {
				_ = d.capture(d.caseID, d.turn, Scripted{Kind: "decision", Response: encoded})
			}
		}
		return response, nil
	}

	if d.consumed >= len(d.fixtures) {
		return jev.DecisionResponse{}, errors.New(
			"unexpected judge call: the case scripted no reply")
	}
	reply := d.fixtures[d.consumed]
	d.consumed++

	switch reply.Kind {
	case "decision":
		var response jev.DecisionResponse
		if err := json.Unmarshal(reply.Response, &response); err != nil {
			return jev.DecisionResponse{}, fmt.Errorf("parsing scripted reply: %w", err)
		}
		return response, nil
	case "decision_error":
		return jev.DecisionResponse{}, errors.New(reply.Message)
	default:
		return jev.DecisionResponse{}, fmt.Errorf("scripted step %q cannot serve a judge call", reply.Kind)
	}
}

// toResult maps a routing outcome onto the oracle's result schema.
//
// Everything is read from the outcome's *rewritten* request, not from the one that
// went in: routing stamps the selected model into it and a processor may splice a
// tier prompt into the instructions, so the input is not what the model would
// receive. The digest comes from libsygo rather than a re-encode, because the oracle
// digests libsy's own bytes and Go would reorder object keys on the way back.
func toResult(caseID string, index int, outcome libsygo.Outcome, decider *turnDecider) TurnResult {
	unconsumed := decider.unconsumed()

	if outcome.Error != "" {
		steps := []string{"Error"}
		if outcome.Calls > 0 {
			steps = []string{"CallDecision", "Error"}
		}
		// A failed turn still showed the judge something, and the oracle records it: the
		// error path is not a reason to lose what was asked.
		result := ErrorResult(caseID, index, steps, unconsumed, outcome.Error)
		result.JudgeTask = decider.judgeTask
		return result
	}

	steps := []string{"Done"}
	if outcome.Calls > 0 {
		steps = []string{"CallDecision", "Done"}
	}
	return TurnResult{
		CaseID:             caseID,
		Turn:               index,
		Steps:              steps,
		StepCount:          len(steps),
		SelectedModelIDs:   outcome.SelectedModelIDs,
		SelectedModelID:    strptr(outcome.Model),
		RewrittenModel:     strptr(outcome.Request.Model),
		Instructions:       libsygo.InstructionsJSON(outcome.Request.Instructions),
		MessagesDigest:     libsygo.MessagesDigest(outcome.Request.Messages),
		ResponseFormat:     outcome.Request.Output.ResponseFormat,
		Evidence:           outcome.Evidence,
		Algorithm:          strptr(outcome.Algorithm),
		JudgeTask:          decider.judgeTask,
		ResponsePresent:    outcome.Responded,
		UnconsumedScripted: unconsumed,
	}
}
