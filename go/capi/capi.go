// Package capi drives switchyard-libsy through its C ABI.
//
// Routing is a pull loop, because libsy hands every model call back to its host:
//
//	algo, err := capi.New("llm_classifier", configJSON)
//	run, err  := algo.Start(requestJSON, modelsJSON, headersJSON)
//	for {
//	    step, err := run.Next()
//	    switch step.Kind {
//	    case capi.StepCallDecision: // or StepCallModel
//	        run.Respond(step.ID, verdictJSON)   // or run.Fail(step.ID, "…")
//	    case capi.StepDone:
//	        ...
//	    }
//	}
//
// The host serves every call, so egress stays host-side: this package never
// opens a socket, and neither does the library behind it.
package capi

/*
#cgo LDFLAGS: -L${SRCDIR}/../../target/debug -lswitchyard_capi
#include <stdlib.h>

char *sy_version(void);
void *sy_algorithm_new(const char *kind, const char *config_json);
void *sy_run_start(void *algo, const char *request_json, const char *models_json, const char *headers_json);
char *sy_run_next(void *run);
int   sy_call_respond(void *run, unsigned long long id, const char *response_json);
int   sy_call_fail(void *run, unsigned long long id, const char *message);
void  sy_string_free(char *value);
void  sy_run_free(void *run);
void  sy_algorithm_free(void *algo);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// Step kinds returned by Run.Next.
const (
	StepCallModel    = "call_model"
	StepCallDecision = "call_decision"
	StepDone         = "done"
	StepEnd          = "end"
	StepError        = "error"
)

// Step is one item from the algorithm's step stream.
type Step struct {
	Kind string `json:"kind"`

	// ID names the pending call to answer with Respond or Fail. Set for
	// call_model and call_decision.
	ID uint64 `json:"id"`

	// Algorithm that produced the call, for attributing host telemetry.
	Algorithm string `json:"algorithm"`

	// Model is the judge target for a call_decision.
	Model string `json:"model"`

	// Request is the normalized request to serve: a DecisionRequest for a
	// call_decision, an LlmRequest for a call_model.
	Request json.RawMessage `json:"request"`

	// Models are the candidate targets for a call_model, in order.
	Models []string `json:"models"`

	// Outcome is set on done.
	Outcome *Outcome `json:"outcome"`

	// Message is set on error.
	Message string `json:"message"`
}

// Outcome is the terminal result of a run.
type Outcome struct {
	// SelectedModelIDs is libsy's fallback order: the chosen model first, then
	// the rest of its category, then every other runtime target.
	SelectedModelIDs []string `json:"selected_model_ids"`

	// Request is the request after routing-time rewrites.
	Request json.RawMessage `json:"request"`

	// ResponsePresent is true when the algorithm produced an answer while
	// routing, rather than the host having to make the final call.
	ResponsePresent bool `json:"response_present"`

	// Metadata carries the outcome id, the algorithm name, and the evidence
	// explaining the decision.
	Metadata json.RawMessage `json:"metadata"`
}

// Algorithm is an owned algorithm. It is not safe to copy.
type Algorithm struct {
	ptr unsafe.Pointer
}

// Run is one in-flight run. It holds the calls awaiting a reply.
type Run struct {
	ptr unsafe.Pointer
}

// Version returns the ABI's version string.
func Version() string {
	return C.GoString(C.sy_version())
}

// New builds an algorithm from a kind and a JSON config.
//
// Kinds currently implemented: "noop" and "llm_classifier" (capability, over a
// decision judge).
func New(kind, configJSON string) (*Algorithm, error) {
	kindC := C.CString(kind)
	defer C.free(unsafe.Pointer(kindC))
	configC := C.CString(configJSON)
	defer C.free(unsafe.Pointer(configC))

	ptr := C.sy_algorithm_new(kindC, configC)
	if ptr == nil {
		return nil, fmt.Errorf("capi: building %q failed (see stderr for the reason)", kind)
	}
	algorithm := &Algorithm{ptr: ptr}
	runtime.SetFinalizer(algorithm, (*Algorithm).Close)
	return algorithm, nil
}

// Close releases the algorithm. Safe to call more than once.
func (a *Algorithm) Close() {
	if a == nil || a.ptr == nil {
		return
	}
	C.sy_algorithm_free(a.ptr)
	a.ptr = nil
	runtime.SetFinalizer(a, nil)
}

// Start begins a run.
//
// requestJSON is `{"llm_request": …, "headers": {…}}`; modelsJSON maps a category
// to model ids, best-first, and must list every routable target under "any";
// headersJSON is optional and is normalized into the request's correlation
// metadata. Pass "" to omit.
func (a *Algorithm) Start(requestJSON, modelsJSON, headersJSON string) (*Run, error) {
	if a == nil || a.ptr == nil {
		return nil, errors.New("capi: algorithm is closed")
	}
	requestC := C.CString(requestJSON)
	defer C.free(unsafe.Pointer(requestC))
	modelsC := C.CString(modelsJSON)
	defer C.free(unsafe.Pointer(modelsC))
	headersC := C.CString(headersJSON)
	defer C.free(unsafe.Pointer(headersC))

	ptr := C.sy_run_start(a.ptr, requestC, modelsC, headersC)
	if ptr == nil {
		return nil, errors.New("capi: starting the run failed (see stderr for the reason)")
	}
	run := &Run{ptr: ptr}
	runtime.SetFinalizer(run, (*Run).Close)
	return run, nil
}

// Next returns the next step, blocking until it is ready.
//
// The loop ends with a StepDone, or a StepError if the algorithm failed.
func (r *Run) Next() (Step, error) {
	if r == nil || r.ptr == nil {
		return Step{}, errors.New("capi: run is closed")
	}
	raw := C.sy_run_next(r.ptr)
	if raw == nil {
		return Step{}, errors.New("capi: the ABI returned no step")
	}
	text := C.GoString(raw)
	C.sy_string_free(raw)

	var step Step
	if err := json.Unmarshal([]byte(text), &step); err != nil {
		return Step{}, fmt.Errorf("capi: decoding step %q: %w", text, err)
	}
	return step, nil
}

// Respond fulfils a pending call. responseJSON is an aggregate model reply
// (`{"llm_response": {...}}`) for a call_model, or a DecisionResponse for a
// call_decision.
func (r *Run) Respond(id uint64, responseJSON string) error {
	if r == nil || r.ptr == nil {
		return errors.New("capi: run is closed")
	}
	responseC := C.CString(responseJSON)
	defer C.free(unsafe.Pointer(responseC))

	if rc := C.sy_call_respond(r.ptr, C.ulonglong(id), responseC); rc != 0 {
		return fmt.Errorf("capi: responding to call %d failed", id)
	}
	return nil
}

// Fail answers a pending call with an error, so the algorithm applies its
// fallback policy rather than the run dying.
func (r *Run) Fail(id uint64, message string) error {
	if r == nil || r.ptr == nil {
		return errors.New("capi: run is closed")
	}
	messageC := C.CString(message)
	defer C.free(unsafe.Pointer(messageC))

	if rc := C.sy_call_fail(r.ptr, C.ulonglong(id), messageC); rc != 0 {
		return fmt.Errorf("capi: failing call %d failed", id)
	}
	return nil
}

// Close releases the run. Safe to call more than once.
func (r *Run) Close() {
	if r == nil || r.ptr == nil {
		return
	}
	C.sy_run_free(r.ptr)
	r.ptr = nil
	runtime.SetFinalizer(r, nil)
}
