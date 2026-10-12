//! Corpus case types and loading.

use std::collections::BTreeMap;
use std::path::Path;

use anyhow::{Context, Result};
use serde::Deserialize;
use serde_json::Value;

use crate::profile::Profile;

/// One corpus case: a profile, a per-request model pool, and one or more turns.
#[derive(Debug, Clone, Deserialize)]
pub struct Case {
    pub case_id: String,
    #[serde(default)]
    pub tier: Option<u8>,
    pub profile: Profile,
    pub models: BTreeMap<String, Vec<String>>,
    #[serde(default)]
    pub subagent_models: Option<BTreeMap<String, Vec<String>>>,

    // Single-turn shorthand. Ignored when `turns` is present.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub headers: BTreeMap<String, String>,
    #[serde(default)]
    pub prompt: Option<String>,
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub stream: Option<bool>,
    #[serde(default)]
    pub decider_responses: Option<DeciderResponses>,

    // Multi-turn sequence sharing one algorithm instance (session state persists).
    #[serde(default)]
    pub turns: Option<Vec<TurnSpec>>,
}

impl Case {
    /// The turns to run: an explicit sequence, or one turn from the shorthand.
    pub fn turn_specs(&self) -> Vec<TurnSpec> {
        if let Some(turns) = &self.turns {
            return turns.clone();
        }
        vec![TurnSpec {
            headers: self.headers.clone(),
            prompt: self.prompt.clone(),
            model: self.model.clone(),
            stream: self.stream,
            decider_responses: self.decider_responses.clone(),
        }]
    }
}

/// One turn within a case.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct TurnSpec {
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    #[serde(default)]
    pub prompt: Option<String>,
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub stream: Option<bool>,
    #[serde(default)]
    pub decider_responses: Option<DeciderResponses>,
}

/// Scripted decider responses, consumed in step order.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct DeciderResponses {
    #[serde(default)]
    pub by_index: Vec<Scripted>,
}

/// A scripted reply for one offloaded step.
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Scripted {
    /// Fulfill a `CallDecision` with a `DecisionResponse`.
    Decision { response: Value },
    /// Fail a `CallDecision` so the algorithm can apply its fallback policy.
    DecisionError { message: String },
    /// Fulfill a `CallModel` with an aggregate `AggLlmResponse`.
    Model { response: Value },
    /// Fail a `CallModel`.
    ModelError { message: String },
}

/// Loads a JSONL suite. Blank lines and `//` comments are ignored.
pub fn load_suite(path: &Path) -> Result<Vec<Case>> {
    let text =
        std::fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
    let mut cases = Vec::new();
    for (index, raw) in text.lines().enumerate() {
        let trimmed = raw.trim();
        if trimmed.is_empty() || trimmed.starts_with("//") {
            continue;
        }
        let case: Case = serde_json::from_str(trimmed)
            .with_context(|| format!("{}:{}: parsing case", path.display(), index + 1))?;
        cases.push(case);
    }
    Ok(cases)
}
