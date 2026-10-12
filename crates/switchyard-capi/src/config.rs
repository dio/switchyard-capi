//! Profile configuration, decoded from JSON at the ABI boundary.
//!
//! libsy's configs are not `serde`, so each algorithm family needs a DTO and a
//! mapping onto its constructors. This module holds the ones the skeleton
//! exercises; the census in `docs/abi-sizing.md` counts the rest.

use std::collections::BTreeMap;

use serde::Deserialize;
use serde_json::Value;
use switchyard_libsy::{
    CapabilityJudgeConfig, ClassifyTrigger, DecisionJudgeConfig, LlmClassifierConfig,
    LlmTaskClassifier, TaskClassifierConfig,
};
use switchyard_protocol::ModelId;

/// The capability classifier over a decision (System One / JEV) judge.
///
/// Eight fields. Note the saving the census predicted: `ClassifyTrigger` already
/// derives `Deserialize`, so it crosses the boundary as itself rather than as a
/// bool the mapping has to translate.
#[derive(Debug, Deserialize)]
pub struct CapabilityDecisionConfig {
    pub cutoff: f64,
    /// `Option` rather than a defaulted map: a JSON producer emits `null` for an
    /// absent map, and `#[serde(default)]` does not cover a present `null`.
    #[serde(default)]
    pub candidates: Option<BTreeMap<String, String>>,
    #[serde(default)]
    pub evidence: Value,
    #[serde(default)]
    pub instructions: Option<Value>,
    #[serde(default = "default_true")]
    pub fail_open: bool,
    #[serde(default)]
    pub classify_trigger: ClassifyTrigger,
    #[serde(default)]
    pub message_hash_fallback: bool,
    #[serde(default)]
    pub recent_turn_window: Option<usize>,
}

fn default_true() -> bool {
    true
}

impl CapabilityDecisionConfig {
    /// Builds the classifier this configuration describes.
    pub fn build(self) -> Result<LlmTaskClassifier, String> {
        let classifier = TaskClassifierConfig {
            judge: CapabilityJudgeConfig::Decision(DecisionJudgeConfig {
                cutoff: self.cutoff,
                instructions: self.instructions,
                candidates: self
                    .candidates
                    .unwrap_or_default()
                    .into_iter()
                    .map(|(label, model)| (label, ModelId::from(model)))
                    .collect(),
                evidence: self.evidence,
            }),
            fail_open: self.fail_open,
            classify_trigger: self.classify_trigger,
            message_hash_fallback: self.message_hash_fallback,
            recent_turn_window: self.recent_turn_window,
        };
        LlmTaskClassifier::new(LlmClassifierConfig::Capability { config: classifier })
            .map_err(|error| error.to_string())
    }
}
