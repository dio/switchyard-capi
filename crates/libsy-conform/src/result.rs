//! Result schema — the oracle's JSON output, and the reference every other
//! implementation is diffed against.
//!
//! Masked on purpose: nothing time-derived (`outcome_id`, durations) is emitted,
//! so a re-run is byte-identical.

use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use switchyard_libsy::RoutingOutcome;

use crate::corpus::Case;

/// The observable result of one turn.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TurnResult {
    pub case_id: String,
    pub turn: usize,
    /// Ordered step kinds: `CallModel` / `CallDecision` / `Done` / `Error`.
    /// A local profile must emit `["Done"]` — no calls.
    pub steps: Vec<String>,
    pub step_count: usize,
    /// Selected models, best-first: the pick, then the rest of its category, then
    /// every other runtime target.
    pub selected_model_ids: Vec<String>,
    pub selected_model_id: Option<String>,
    /// The model stamped into the request after routing.
    pub rewritten_model: Option<String>,
    /// Instructions after routing — captures tier prompts injected by processors.
    pub instructions: Value,
    /// Digest of the messages after routing — captures handoff notes.
    pub messages_digest: String,
    /// Algorithm evidence: `{source, verdict, score, threshold}` or a fail-open
    /// `{source, reason_code}`.
    pub evidence: Option<Value>,
    pub algorithm: Option<String>,
    pub response_present: bool,
    /// Scripted responses the run never asked for. Non-zero means the route took a
    /// different path than the case anticipated (e.g. an affinity latch skipped a
    /// judge call) — recorded rather than fatal, so semantics can be discovered.
    pub unconsumed_scripted: usize,
    pub error: Option<String>,
}

impl TurnResult {
    pub fn from_run(
        case: &Case,
        turn: usize,
        steps: Vec<String>,
        outcome: Option<&RoutingOutcome>,
        unconsumed_scripted: usize,
        error: Option<String>,
    ) -> Self {
        let step_count = steps.len();
        let Some(outcome) = outcome else {
            return Self {
                case_id: case.case_id.clone(),
                turn,
                steps,
                step_count,
                selected_model_ids: Vec::new(),
                selected_model_id: None,
                rewritten_model: None,
                instructions: Value::Null,
                messages_digest: String::new(),
                evidence: None,
                algorithm: None,
                response_present: false,
                unconsumed_scripted,
                error,
            };
        };

        let llm_request = &outcome.request.llm_request;
        Self {
            case_id: case.case_id.clone(),
            turn,
            steps,
            step_count,
            selected_model_ids: outcome
                .selected_model_ids
                .iter()
                .map(ToString::to_string)
                .collect(),
            selected_model_id: outcome.selected_model_ids.first().map(ToString::to_string),
            rewritten_model: llm_request.model.clone(),
            instructions: serde_json::to_value(&llm_request.instructions).unwrap_or(Value::Null),
            messages_digest: digest(
                &serde_json::to_value(&llm_request.messages).unwrap_or(Value::Null),
            ),
            evidence: outcome.metadata.as_ref().and_then(|m| m.evidence.clone()),
            algorithm: outcome.metadata.as_ref().map(|m| m.algorithm.clone()),
            response_present: outcome.response.is_some(),
            unconsumed_scripted,
            error,
        }
    }
}

/// Stable digest of a JSON value, used where a full field-by-field diff would be
/// noisy (message bodies).
pub fn digest(value: &Value) -> String {
    let canonical = serde_json::to_string(value).unwrap_or_default();
    let mut hasher = Sha256::new();
    hasher.update(canonical.as_bytes());
    format!("sha256:{:x}", hasher.finalize())
}
