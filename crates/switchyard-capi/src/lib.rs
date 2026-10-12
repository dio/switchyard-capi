//! C ABI for `switchyard-libsy`.
//!
//! For hosts that cannot link Rust directly (Go, via cgo). Routing is a **pull
//! loop**, because libsy hands every model call back to its host:
//!
//! ```text
//!   sy_algorithm_new(kind, config_json)            -> *mut AlgoHandle
//!   sy_run_start(algo, request, models, headers)   -> *mut RunHandle
//!   sy_run_next(run)                               -> *mut c_char   (Step JSON)
//!   sy_call_respond(run, id, response_json)        -> i32            (fulfil)
//!   sy_call_fail(run, id, message)                 -> i32            (fail)
//!   sy_run_free / sy_algorithm_free / sy_string_free
//! ```
//!
//! `sy_run_next` blocks: the host drives the loop, so no callback ever crosses
//! back into the host, and this crate never opens a socket for a model call —
//! the host serves those over whatever egress it owns.

use std::collections::{BTreeMap, HashMap};
use std::ffi::{c_char, CStr, CString};
use std::sync::{Arc, OnceLock};

use futures::StreamExt;
use http::HeaderMap;
use serde::Deserialize;
use serde_json::{json, Value};
use switchyard_libsy::{
    Algorithm, CallDecision, CallModel, LibsyError, Noop, RuntimeModels, Step, StepStream,
};
use switchyard_protocol::{
    AggLlmResponse, Category, DecisionResponse, LlmRequest, LlmResponse, Metadata, ModelId,
    Request, Response,
};

mod config;

/// The one tokio runtime. `run_stream` spawns the algorithm task, so it needs a
/// runtime context; every blocking call site gets one from here.
fn runtime() -> &'static tokio::runtime::Runtime {
    static RUNTIME: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RUNTIME.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .expect("switchyard-capi: building the tokio runtime")
    })
}

/// An owned algorithm. Opaque to the host.
pub struct AlgoHandle {
    algorithm: Arc<dyn Algorithm>,
}

/// One in-flight run: the step stream, plus the calls awaiting the host's reply.
pub struct RunHandle {
    stream: StepStream,
    pending: HashMap<u64, Pending>,
    next_id: u64,
}

enum Pending {
    Model(Box<CallModel>),
    Decision(Box<CallDecision>),
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

/// Returns a static version string. Never freed.
#[no_mangle]
pub extern "C" fn sy_version() -> *const c_char {
    static VERSION: &str = concat!(env!("CARGO_PKG_VERSION"), "\0");
    VERSION.as_ptr() as *const c_char
}

/// Builds an algorithm from a kind and a JSON config. Returns null on failure,
/// after logging the reason to stderr.
///
/// # Safety
/// `kind` and `config_json` must be NUL-terminated or null.
#[no_mangle]
pub unsafe extern "C" fn sy_algorithm_new(
    kind: *const c_char,
    config_json: *const c_char,
) -> *mut AlgoHandle {
    let Some(kind) = cstr(kind) else {
        eprintln!("switchyard-capi: sy_algorithm_new: kind must not be null");
        return std::ptr::null_mut();
    };
    let config = cstr(config_json).unwrap_or_default();

    match build_algorithm(&kind, &config) {
        Ok(algorithm) => Box::into_raw(Box::new(AlgoHandle { algorithm })),
        Err(error) => {
            eprintln!("switchyard-capi: sy_algorithm_new({kind}): {error}");
            std::ptr::null_mut()
        }
    }
}

fn build_algorithm(kind: &str, config_json: &str) -> Result<Arc<dyn Algorithm>, String> {
    match kind {
        // No configuration at all: the baseline for per-field cost.
        "noop" => Ok(Arc::new(Noop {})),

        // The DTO path: eight fields across four nested config types.
        "llm_classifier" => {
            let config: config::CapabilityDecisionConfig = decode(config_json)?;
            Ok(Arc::new(config.build()?))
        }

        other => Err(format!("unknown algorithm kind {other:?}")),
    }
}

// ---------------------------------------------------------------------------
// Running
// ---------------------------------------------------------------------------

/// Starts a run. Returns null on failure.
///
/// # Safety
/// `algo` must come from `sy_algorithm_new` and not have been freed.
#[no_mangle]
pub unsafe extern "C" fn sy_run_start(
    algo: *mut AlgoHandle,
    request_json: *const c_char,
    models_json: *const c_char,
    headers_json: *const c_char,
) -> *mut RunHandle {
    if algo.is_null() {
        eprintln!("switchyard-capi: sy_run_start: algorithm must not be null");
        return std::ptr::null_mut();
    }

    let request = match build_request(request_json, headers_json) {
        Ok(request) => request,
        Err(error) => {
            eprintln!("switchyard-capi: sy_run_start: {error}");
            return std::ptr::null_mut();
        }
    };
    let models = match build_models(models_json) {
        Ok(models) => Arc::new(models),
        Err(error) => {
            eprintln!("switchyard-capi: sy_run_start: {error}");
            return std::ptr::null_mut();
        }
    };

    let algorithm = (*algo).algorithm.clone();
    // `run_stream` spawns the algorithm task, so it must be created with a runtime
    // context available.
    let guard = runtime().enter();
    let stream = algorithm.run_stream(request, models);
    drop(guard);

    Box::into_raw(Box::new(RunHandle {
        stream,
        pending: HashMap::new(),
        next_id: 0,
    }))
}

/// Returns the next step as JSON, blocking until it is ready. Free the result
/// with `sy_string_free`.
///
/// Shapes: `{"kind":"call_model",...}`, `{"kind":"call_decision",...}`,
/// `{"kind":"done","outcome":{...}}`, `{"kind":"error","message":...}`.
///
/// # Safety
/// `run` must come from `sy_run_start` and not have been freed.
#[no_mangle]
pub unsafe extern "C" fn sy_run_next(run: *mut RunHandle) -> *mut c_char {
    if run.is_null() {
        return std::ptr::null_mut();
    }
    let run = &mut *run;

    match runtime().block_on(run.stream.next()) {
        None => json_c(json!({"kind": "end"})),
        Some(Ok(step)) => json_c(encode_step(run, step)),
        Some(Err(error)) => json_c(json!({"kind": "error", "message": error.to_string()})),
    }
}

fn encode_step(run: &mut RunHandle, step: Step) -> Value {
    match step {
        Step::CallModel(call) => {
            let id = run.take_id();
            let payload = json!({
                "kind": "call_model",
                "id": id,
                "algorithm": call.algorithm,
                "request": call.request.llm_request,
                "models": call.models.iter().map(ToString::to_string).collect::<Vec<_>>(),
                "recover_errors": call.recover_errors,
            });
            run.pending.insert(id, Pending::Model(call));
            payload
        }
        Step::CallDecision(call) => {
            let id = run.take_id();
            let payload = json!({
                "kind": "call_decision",
                "id": id,
                "algorithm": call.algorithm,
                "model": call.model.to_string(),
                "request": call.request,
            });
            run.pending.insert(id, Pending::Decision(call));
            payload
        }
        Step::Done(outcome) => {
            let metadata = outcome.metadata.as_ref().map(|metadata| {
                json!({
                    "algorithm": metadata.algorithm,
                    "outcome_id": metadata.outcome_id(),
                    "evidence": metadata.evidence,
                })
            });
            json!({
                "kind": "done",
                "outcome": {
                    "selected_model_ids": outcome
                        .selected_model_ids
                        .iter()
                        .map(ToString::to_string)
                        .collect::<Vec<_>>(),
                    "request": outcome.request.llm_request,
                    "response_present": outcome.response.is_some(),
                    "metadata": metadata,
                }
            })
        }
    }
}

impl RunHandle {
    fn take_id(&mut self) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        id
    }

    fn take_pending(&mut self, id: u64) -> Result<Pending, String> {
        self.pending
            .remove(&id)
            .ok_or_else(|| format!("no call pending with id {id}"))
    }
}

/// Fulfils a `call_model` or `call_decision` step with a JSON reply. Returns 0 on
/// success and a non-zero value on failure.
///
/// # Safety
/// `run` must be live and `id` must name a pending call.
#[no_mangle]
pub unsafe extern "C" fn sy_call_respond(
    run: *mut RunHandle,
    id: u64,
    response_json: *const c_char,
) -> i32 {
    if run.is_null() {
        return 1;
    }
    let run = &mut *run;
    let text = cstr(response_json).unwrap_or_default();

    let result = match run.take_pending(id) {
        Err(error) => Err(error),
        Ok(Pending::Model(call)) => {
            #[derive(Deserialize)]
            struct ModelReply {
                llm_response: AggLlmResponse,
            }
            match serde_json::from_str::<ModelReply>(body(&text)) {
                Err(error) => Err(format!("decoding model reply: {error}")),
                Ok(reply) => call
                    .respond(Ok(Response {
                        llm_response: LlmResponse::Agg(reply.llm_response),
                        metadata: None,
                        upstream_headers: HeaderMap::new(),
                    }))
                    .map_err(|error| error.to_string()),
            }
        }
        Ok(Pending::Decision(call)) => {
            match serde_json::from_str::<DecisionResponse>(body(&text)) {
                Err(error) => Err(format!("decoding decision reply: {error}")),
                Ok(reply) => call.respond(Ok(reply)).map_err(|error| error.to_string()),
            }
        }
    };

    match result {
        Ok(()) => 0,
        Err(error) => {
            eprintln!("switchyard-capi: sy_call_respond: {error}");
            1
        }
    }
}

/// Fails a pending call, so the algorithm applies its fallback policy. Returns 0
/// on success.
///
/// # Safety
/// `run` must be live and `id` must name a pending call.
#[no_mangle]
pub unsafe extern "C" fn sy_call_fail(run: *mut RunHandle, id: u64, message: *const c_char) -> i32 {
    if run.is_null() {
        return 1;
    }
    let run = &mut *run;
    let message = cstr(message).unwrap_or_else(|| "host reported a failure".to_string());

    match run.take_pending(id) {
        Err(error) => {
            eprintln!("switchyard-capi: sy_call_fail: {error}");
            1
        }
        Ok(Pending::Model(call)) => u32::from(
            call.respond(Err(LibsyError::AlgorithmError {
                message: message.clone(),
            }))
            .is_err(),
        ) as i32,
        Ok(Pending::Decision(call)) => u32::from(
            call.respond(Err(LibsyError::AlgorithmError { message }))
                .is_err(),
        ) as i32,
    }
}

// ---------------------------------------------------------------------------
// Freeing
// ---------------------------------------------------------------------------

/// Frees a string returned by this library.
///
/// # Safety
/// `value` must be null or a pointer returned by this library.
#[no_mangle]
pub unsafe extern "C" fn sy_string_free(value: *mut c_char) {
    if !value.is_null() {
        drop(CString::from_raw(value));
    }
}

/// Frees a run.
///
/// # Safety
/// `run` must be null or a pointer from `sy_run_start`, freed at most once.
#[no_mangle]
pub unsafe extern "C" fn sy_run_free(run: *mut RunHandle) {
    if !run.is_null() {
        drop(Box::from_raw(run));
    }
}

/// Frees an algorithm.
///
/// # Safety
/// `algo` must be null or a pointer from `sy_algorithm_new`, freed at most once.
#[no_mangle]
pub unsafe extern "C" fn sy_algorithm_free(algo: *mut AlgoHandle) {
    if !algo.is_null() {
        drop(Box::from_raw(algo));
    }
}

// ---------------------------------------------------------------------------
// Input decoding
// ---------------------------------------------------------------------------

unsafe fn cstr(ptr: *const c_char) -> Option<String> {
    if ptr.is_null() {
        return None;
    }
    Some(CStr::from_ptr(ptr).to_string_lossy().into_owned())
}

fn body(text: &str) -> &str {
    if text.trim().is_empty() {
        "{}"
    } else {
        text
    }
}

/// Decodes boundary JSON, quoting the input when it fails: a host debugging a cgo
/// call has no other view of what crossed.
fn decode<T: for<'de> Deserialize<'de>>(text: &str) -> Result<T, String> {
    let input = body(text);
    serde_json::from_str(input).map_err(|error| {
        let shown: String = input.chars().take(160).collect();
        format!("decoding JSON: {error} (input: {shown})")
    })
}

/// The request envelope. `llm_request` is libsy's normalized request; production
/// hosts build it with `switchyard-translation`.
///
/// `headers` is `Option` rather than a defaulted map because JSON producers
/// naturally emit `null` for an absent map, and `#[serde(default)]` only covers a
/// *missing* key — not a present `null`.
#[derive(Deserialize)]
struct RequestInput {
    llm_request: LlmRequest,
    #[serde(default)]
    headers: Option<BTreeMap<String, String>>,
}

unsafe fn build_request(
    request_json: *const c_char,
    headers_json: *const c_char,
) -> Result<Request, String> {
    let text = cstr(request_json).ok_or("request_json must not be null")?;
    let input: RequestInput = decode(&text)?;

    // Headers may also arrive as their own argument, when the host keeps them out
    // of the request body.
    let from_argument = match cstr(headers_json) {
        Some(json) if !json.trim().is_empty() => {
            let parsed: BTreeMap<String, String> = decode(&json)?;
            Some(parsed)
        }
        _ => None,
    };
    let headers = from_argument.or(input.headers).filter(|h| !h.is_empty());
    let metadata = headers.map(|headers| {
        let mut map = HeaderMap::new();
        for (name, value) in headers {
            if let (Ok(name), Ok(value)) = (
                http::HeaderName::from_bytes(name.as_bytes()),
                http::HeaderValue::from_str(&value),
            ) {
                map.insert(name, value);
            }
        }
        Metadata::from_headers(&map)
    });

    Ok(Request {
        llm_request: input.llm_request,
        raw_request: None,
        metadata,
    })
}

fn build_models(models_json: *const c_char) -> Result<RuntimeModels, String> {
    let text = unsafe { cstr(models_json) }.ok_or("models_json must not be null")?;
    let parsed: BTreeMap<String, Vec<String>> = decode(&text)?;

    validate_pool(&parsed)?;

    let mut by_category = HashMap::new();
    for (name, ids) in parsed {
        let category = name
            .parse::<Category>()
            .map_err(|error| format!("invalid model category {name:?}: {error}"))?;
        by_category.insert(
            category,
            ids.into_iter().map(ModelId::from).collect::<Vec<_>>(),
        );
    }
    Ok(RuntimeModels::new(by_category))
}

/// Mirrors `switchyard-runner`'s check, and the Go port's, so both hosts reject
/// the same pools at the same point.
///
/// libsy itself only notices a missing `any` while routing (`TargetNotFound`),
/// because the selected target is resolved against it. Rejecting earlier turns a
/// mid-route failure into a construction error, and keeps the two
/// implementations agreeing under the differential harness.
fn validate_pool(models: &BTreeMap<String, Vec<String>>) -> Result<(), String> {
    let any = models.get("any").map(Vec::as_slice).unwrap_or(&[]);
    if any.is_empty() {
        return Err(
            "models.any must list every routable target: the selected target is resolved against it"
                .to_string(),
        );
    }
    for (name, ids) in models {
        if name == "any" || name == "judge" {
            continue;
        }
        for id in ids {
            if !any.contains(id) {
                return Err(format!(
                    "models.{name} lists {id:?}, which must also appear in models.any"
                ));
            }
        }
    }
    Ok(())
}

fn json_c(value: Value) -> *mut c_char {
    match CString::new(value.to_string()) {
        Ok(text) => text.into_raw(),
        Err(_) => std::ptr::null_mut(),
    }
}
