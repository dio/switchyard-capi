package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// TurnResult is the oracle's result schema. Field set and null-vs-empty choices
// must match the Rust side exactly, because the diff is on parsed JSON.
type TurnResult struct {
	CaseID             string          `json:"case_id"`
	Turn               int             `json:"turn"`
	Steps              []string        `json:"steps"`
	StepCount          int             `json:"step_count"`
	SelectedModelIDs   []string        `json:"selected_model_ids"`
	SelectedModelID    *string         `json:"selected_model_id"`
	RewrittenModel     *string         `json:"rewritten_model"`
	Instructions       json.RawMessage `json:"instructions"`
	MessagesDigest     string          `json:"messages_digest"`
	Evidence           json.RawMessage `json:"evidence"`
	Algorithm          *string         `json:"algorithm"`
	ResponsePresent    bool            `json:"response_present"`
	UnconsumedScripted int             `json:"unconsumed_scripted"`
	Error              *string         `json:"error"`
}

// ErrorResult is the shape used when a run ends without a routing outcome. The
// Rust side emits `null` instructions and an empty digest here, not `[]`.
func ErrorResult(caseID string, turn int, steps []string, unconsumed int, message string) TurnResult {
	return TurnResult{
		CaseID:             caseID,
		Turn:               turn,
		Steps:              steps,
		StepCount:          len(steps),
		SelectedModelIDs:   []string{},
		Instructions:       nil, // -> null
		Evidence:           nil, // -> null
		UnconsumedScripted: unconsumed,
		Error:              &message,
	}
}

// message/contentBlock mirror libsy's `Message`/`ContentBlock::Text` serde shape
// so the digest is byte-identical to Rust's compact serde_json output.
type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MessagesDigest reproduces the oracle's digest: sha256 over compact JSON of the
// normalized text-only conversation, matching serde_json's output.
func MessagesDigest(prompt string) string {
	if prompt == "" {
		return ""
	}
	encoded, err := json.Marshal([]message{{
		Role:    "user",
		Content: []contentBlock{{Type: "text", Text: prompt}},
	}})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func strptr(s string) *string { return &s }
