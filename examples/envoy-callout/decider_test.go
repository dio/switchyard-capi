package envoycallout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dio/switchyard-capi/go/decision"
	"github.com/dio/switchyard-capi/go/jev"
)

// judgeReply is the shape the live System One endpoint returns, captured from a
// real call.
const judgeReply = `{"model":"jev-1.13.0","answers":{"route":{"type":"choice","choice":"advantage","confidence":0.9,"probabilities":{"advantage":0.77,"no_advantage":0.23}}},"usage":{"input_tokens":842,"output_tokens":39}}`

type fakeCallout struct {
	requests []CalloutRequest
	reply    []byte
	err      error
}

func (f *fakeCallout) Do(_ context.Context, request CalloutRequest) ([]byte, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	return f.reply, nil
}

func judgeRequest(t *testing.T) jev.DecisionRequest {
	t.Helper()
	options, err := json.Marshal(map[string]any{"options": []map[string]any{
		{"id": "advantage", "description": "capable only"},
		{"id": "no_advantage", "description": "otherwise"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	model := "jev-latest"
	return jev.DecisionRequest{
		Model:   &model,
		Context: json.RawMessage(`{"task":[]}`),
		Questions: map[string]jev.DecisionQuestion{
			"route": {Kind: jev.Tagged{Type: "choice", Data: options}},
		},
	}
}

func TestDeciderEncodesAndDecodes(t *testing.T) {
	callout := &fakeCallout{reply: []byte(judgeReply)}
	decider := NewDecider(callout, "judge", "/v1/systemone", "api.typesafe.ai", 5*time.Second)

	response, err := decider.Call(context.Background(), judgeRequest(t))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	answer, ok := response.Answers["route"]
	if !ok {
		t.Fatal("missing route answer")
	}
	if answer.Value.Type != "choice" {
		t.Fatalf("value type = %q", answer.Value.Type)
	}
	var data struct {
		Selected      string             `json:"selected"`
		Probabilities map[string]float64 `json:"probabilities"`
	}
	if err := json.Unmarshal(answer.Value.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Selected != "advantage" || data.Probabilities["advantage"] != 0.77 {
		t.Fatalf("decoded verdict = %+v", data)
	}
}

// The callout is host-owned: this package decides the cluster, path, authority and
// timeout, and the host only supplies the transport.
func TestDeciderSendsHostOwnedCalloutShape(t *testing.T) {
	callout := &fakeCallout{reply: []byte(judgeReply)}
	decider := NewDecider(callout, "judge-cluster", "/v1/systemone", "api.typesafe.ai", 2500*time.Millisecond)

	if _, err := decider.Call(context.Background(), judgeRequest(t)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(callout.requests) != 1 {
		t.Fatalf("callout made %d times, want 1", len(callout.requests))
	}
	sent := callout.requests[0]
	if sent.Cluster != "judge-cluster" {
		t.Fatalf("cluster = %q", sent.Cluster)
	}
	if sent.Timeout != 2500*time.Millisecond {
		t.Fatalf("timeout = %s", sent.Timeout)
	}
	headers := map[string]string{}
	for _, header := range sent.Headers {
		headers[header[0]] = header[1]
	}
	for name, want := range map[string]string{
		":method": "POST", ":path": "/v1/systemone", ":scheme": "https",
		":authority": "api.typesafe.ai", "content-type": "application/json",
	} {
		if headers[name] != want {
			t.Fatalf("header %s = %q, want %q", name, headers[name], want)
		}
	}
	if !strings.Contains(string(sent.Body), `"jev-latest"`) {
		t.Fatalf("body did not carry the judge model: %s", sent.Body)
	}
}

func TestDeciderSurfacesEncodeFailure(t *testing.T) {
	callout := &fakeCallout{reply: []byte(judgeReply)}
	decider := NewDecider(callout, "judge", "/v1/systemone", "api.typesafe.ai", time.Second)

	request := judgeRequest(t)
	request.Model = nil // System One requires a selected model
	if _, err := decider.Call(context.Background(), request); err == nil {
		t.Fatal("expected an encode failure")
	}
	if len(callout.requests) != 0 {
		t.Fatal("callout should not run when the request cannot be encoded")
	}
}

func TestDeciderSurfacesCalloutFailure(t *testing.T) {
	callout := &fakeCallout{err: errors.New("cluster not found")}
	decider := NewDecider(callout, "judge", "/v1/systemone", "api.typesafe.ai", time.Second)

	_, err := decider.Call(context.Background(), judgeRequest(t))
	if err == nil || !strings.Contains(err.Error(), "cluster not found") {
		t.Fatalf("err = %v, want the callout failure", err)
	}
}

// A reply that does not match the request must fail loudly rather than route on a
// guess.
func TestDeciderSurfacesDecodeFailure(t *testing.T) {
	callout := &fakeCallout{reply: []byte(`{"answers":{"other":{"type":"choice","choice":"advantage"}}}`)}
	decider := NewDecider(callout, "judge", "/v1/systemone", "api.typesafe.ai", time.Second)

	if _, err := decider.Call(context.Background(), judgeRequest(t)); err == nil {
		t.Fatal("expected a decode failure for a mismatched answer key")
	}
}

// The point of the seam: a host callout alone is enough to drive the real router.
func TestDeciderDrivesTheRouter(t *testing.T) {
	callout := &fakeCallout{reply: []byte(judgeReply)}
	router, err := decision.New(
		decision.Profile{
			Algorithm: "llm_classifier",
			Mode:      "capability",
			Judge: &decision.Judge{
				Type:       "decision",
				Cutoff:     0.4,
				Candidates: map[string]string{"a": "smart", "b": "cheap"},
			},
		},
		decision.Models{
			"capable":   {"smart"},
			"efficient": {"cheap"},
			"judge":     {"jev-latest"},
			"any":       {"cheap", "smart"},
		},
		NewDecider(callout, "judge-cluster", "/v1/systemone", "api.typesafe.ai", 5*time.Second),
	)
	if err != nil {
		t.Fatalf("decision.New: %v", err)
	}

	outcome := router.Route(context.Background(), decision.Turn{
		Prompt:  "Refactor the payment module onto the new ledger abstraction.",
		Session: "tenant-a/s-1",
	})
	if outcome.Model != "smart" {
		t.Fatalf("model = %q, want smart (advantage 0.77 > cutoff 0.4)", outcome.Model)
	}
	if len(outcome.SelectedModelIDs) != 2 || outcome.SelectedModelIDs[0] != "smart" {
		t.Fatalf("fallback order = %v", outcome.SelectedModelIDs)
	}
	if !strings.Contains(string(outcome.Evidence), `"score":0.77`) {
		t.Fatalf("evidence = %s", outcome.Evidence)
	}
}
