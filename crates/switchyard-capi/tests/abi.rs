//! Drives the C ABI the way a host would: construct, pull steps, fulfil them.
//!
//! These call the `extern "C"` surface directly rather than through cgo, so no Go
//! toolchain is needed to measure or verify the ABI.

use std::ffi::{c_char, CStr, CString};

use serde_json::{json, Value};
use switchyard_capi::{
    sy_algorithm_free, sy_algorithm_new, sy_call_fail, sy_call_respond, sy_run_free, sy_run_next,
    sy_run_start, sy_string_free,
};

fn c(text: &str) -> CString {
    CString::new(text).expect("CString")
}

/// Reads a returned string, frees it, and parses it as JSON.
unsafe fn take(ptr: *mut c_char) -> Value {
    assert!(!ptr.is_null(), "the ABI returned null");
    let text = CStr::from_ptr(ptr).to_string_lossy().into_owned();
    sy_string_free(ptr);
    serde_json::from_str(&text).expect("step is JSON")
}

const AUTO_REQUEST: &str = r#"{"llm_request":{"model":"auto","messages":[
    {"role":"user","content":[{"type":"text","text":"refactor the payment module"}]}]}}"#;

const CAPABILITY_MODELS: &str =
    r#"{"capable":["smart"],"efficient":["cheap"],"judge":["jev-latest"],"any":["cheap","smart"]}"#;

fn capability_config() -> CString {
    c(&json!({
        "cutoff": 0.4,
        "candidates": {"a": "smart", "b": "cheap"},
        "evidence": {"candidates": {"a": "capable tier", "b": "efficient tier"}}
    })
    .to_string())
}

/// The DTO-free baseline: no config fields, and the run completes without any
/// host call.
#[test]
fn noop_completes_without_host_calls() {
    let kind = c("noop");
    let config = c("{}");
    let models = c(r#"{"any":["m/any"]}"#);
    let request = c(AUTO_REQUEST);

    unsafe {
        let algo = sy_algorithm_new(kind.as_ptr(), config.as_ptr());
        assert!(!algo.is_null());
        let run = sy_run_start(algo, request.as_ptr(), models.as_ptr(), std::ptr::null());
        assert!(!run.is_null());

        let step = take(sy_run_next(run));
        assert_eq!(step["kind"], "done");
        assert_eq!(step["outcome"]["selected_model_ids"][0], "auto");

        sy_run_free(run);
        sy_algorithm_free(algo);
    }
}

/// The DTO path end to end: the config crosses as JSON, the judge call comes back
/// as a step, the host fulfils it, and the run routes on the reply.
#[test]
fn decision_judge_pulls_a_call_and_routes_on_the_reply() {
    let kind = c("llm_classifier");
    let config = capability_config();
    let models = c(CAPABILITY_MODELS);
    let request = c(AUTO_REQUEST);

    unsafe {
        let algo = sy_algorithm_new(kind.as_ptr(), config.as_ptr());
        assert!(!algo.is_null(), "constructing the classifier failed");
        let run = sy_run_start(algo, request.as_ptr(), models.as_ptr(), std::ptr::null());
        assert!(!run.is_null());

        let step = take(sy_run_next(run));
        assert_eq!(step["kind"], "call_decision");
        assert_eq!(step["model"], "jev-latest");
        // The neutral IR nests the tagged enum: `route.kind.type`, unlike System
        // One's flat wire shape (`route.type`).
        assert_eq!(
            step["request"]["questions"]["route"]["kind"]["type"],
            "choice"
        );
        let id = step["id"].as_u64().expect("a pending call id");

        let reply = c(
            &json!({"answers": {"route": {"value": {"type": "choice", "data": {
                "selected": "advantage",
                "probabilities": {"advantage": 0.77, "no_advantage": 0.23}
            }}}}})
            .to_string(),
        );
        assert_eq!(sy_call_respond(run, id, reply.as_ptr()), 0);

        let done = take(sy_run_next(run));
        assert_eq!(done["kind"], "done");
        assert_eq!(
            done["outcome"]["selected_model_ids"][0], "smart",
            "0.77 is above the 0.4 cutoff"
        );
        assert_eq!(done["outcome"]["metadata"]["evidence"]["score"], 0.77);

        sy_run_free(run);
        sy_algorithm_free(algo);
    }
}

/// A host failure is handed back so the algorithm applies its fallback policy,
/// rather than the run dying at the boundary.
#[test]
fn a_failed_call_falls_open() {
    let kind = c("llm_classifier");
    let config = capability_config();
    let models = c(CAPABILITY_MODELS);
    let request = c(AUTO_REQUEST);

    unsafe {
        let algo = sy_algorithm_new(kind.as_ptr(), config.as_ptr());
        let run = sy_run_start(algo, request.as_ptr(), models.as_ptr(), std::ptr::null());

        let step = take(sy_run_next(run));
        let id = step["id"].as_u64().expect("a pending call id");

        let failure = c("judge unavailable");
        assert_eq!(sy_call_fail(run, id, failure.as_ptr()), 0);

        let done = take(sy_run_next(run));
        assert_eq!(done["kind"], "done");
        assert_eq!(
            done["outcome"]["selected_model_ids"][0], "smart",
            "fail-open defaults to the capable tier"
        );
        assert_eq!(
            done["outcome"]["metadata"]["evidence"]["source"],
            "fail_open"
        );

        sy_run_free(run);
        sy_algorithm_free(algo);
    }
}

/// A bad kind or config fails loudly at construction instead of producing an
/// algorithm that cannot route.
#[test]
fn bad_configuration_fails_at_construction() {
    let models = c(CAPABILITY_MODELS);
    let request = c(AUTO_REQUEST);

    unsafe {
        let unknown = c("not-an-algorithm");
        assert!(sy_algorithm_new(unknown.as_ptr(), std::ptr::null()).is_null());

        let kind = c("llm_classifier");
        let invalid = c(r#"{"cutoff":"not a number"}"#);
        assert!(sy_algorithm_new(kind.as_ptr(), invalid.as_ptr()).is_null());

        // And a valid algorithm still refuses an unusable pool.
        let config = capability_config();
        let algo = sy_algorithm_new(kind.as_ptr(), config.as_ptr());
        let no_any = c(r#"{"capable":["smart"]}"#);
        let run = sy_run_start(algo, request.as_ptr(), no_any.as_ptr(), std::ptr::null());
        assert!(run.is_null(), "a pool without `any` must be rejected");
        sy_algorithm_free(algo);

        let _ = (&models, &request);
    }
}
