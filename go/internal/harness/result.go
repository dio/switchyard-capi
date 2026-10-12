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
	ResponseFormat     json.RawMessage `json:"response_format"`
	Evidence           json.RawMessage `json:"evidence"`
	Algorithm          *string         `json:"algorithm"`
	ResponsePresent    bool            `json:"response_present"`
	UnconsumedScripted int             `json:"unconsumed_scripted"`
	Error              *string         `json:"error"`

	// detail is why a rejection happened, in full. Never serialized and never
	// diffed: the recorded error is the label, this is for the operator.
	detail string `json:"-"`
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

// requestRejected is the label the oracle records when a request is refused
// before it can route. The provider's wording is deliberately not compared: two
// implementations will not phrase a rejection identically, so diffing it would
// fail on a difference that is not a behaviour.
const requestRejected = "request_rejected"

// RejectedResult is a request the port declines to decode. It must match the
// oracle's label for input libsy refuses, which is what makes "rejected rather
// than coerced" a comparable behaviour instead of an unstated assumption.
func RejectedResult(caseID string, turn int, detail string) TurnResult {
	result := ErrorResult(caseID, turn, []string{"Error"}, 0, requestRejected)
	result.detail = detail
	return result
}

func strptr(s string) *string { return &s }

// digestRaw hashes a JSON value's exact bytes. Used where the value arrives
// already serialized by libsy: re-encoding it through Go would reorder object
// keys and change the digest.
func digestRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
