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
    /// The structured-output format the request asked for, when it asked for one. A
    /// dropped or unrecognized format records as null, which is the behaviour worth
    /// pinning: a request that asked for a schema and did not get one should be visible.
    pub response_format: Option<Value>,
    /// The messages the judge was actually shown, from the first decision call of the
    /// turn. Null when no judge was consulted, which is its own fact — a latched session
    /// and a signal-driven turn both record it.
    ///
    /// Recorded because it is a *selection*, not a projection: the judge is shown the
    /// opening task and the latest user turn rather than the transcript, and nothing else
    /// in the result distinguishes that from the whole conversation.
    pub judge_task: Option<Value>,
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
    /// Why a rejection happened, in full. Never diffed and never serialized: the
    /// recorded `error` is the label, this is for the operator's eyes.
    #[serde(skip)]
    pub detail: Option<String>,
}

/// The label recorded when a case fails before it can route: an undecodable
/// request, or a profile libsy refuses.
///
/// Deliberately not the provider's message. Two implementations cannot be expected
/// to word a rejection identically, so diffing the wording would fail every port on
/// a difference that is not behavioural. What must match is that the input was
/// *rejected* rather than silently coerced — which is the thing a port gets wrong.
pub const REQUEST_REJECTED: &str = "request_rejected";

impl TurnResult {
    /// A case that could not be run at all. Recorded as one errored turn so the
    /// rest of the suite still runs and the rejection is diffable.
    pub fn rejected(case: &Case, error: &anyhow::Error) -> Self {
        Self::from_run(
            case,
            0,
            vec!["Error".to_string()],
            None,
            0,
            Some(REQUEST_REJECTED.to_string()),
        )
        // The full message still reaches the operator, just not the diff.
        .with_detail(format!("{error:#}"))
    }

    /// Attaches the undiffed detail a human needs to act on a rejection.
    fn with_detail(mut self, detail: String) -> Self {
        self.detail = Some(detail);
        self
    }

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
                response_format: None,
                judge_task: None,
                evidence: None,
                algorithm: None,
                response_present: false,
                unconsumed_scripted,
                error,
                detail: None,
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
            response_format: llm_request.output.response_format.clone(),
            judge_task: None,
            evidence: outcome.metadata.as_ref().and_then(|m| m.evidence.clone()),
            algorithm: outcome.metadata.as_ref().map(|m| m.algorithm.clone()),
            response_present: outcome.response.is_some(),
            unconsumed_scripted,
            error,
            detail: None,
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
