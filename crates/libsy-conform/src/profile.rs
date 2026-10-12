//! Profile JSON → `libsy` algorithm builders.
//!
//! `libsy`'s config structs are not `serde`-derived, so this mirrors what the
//! Python binding does: a small deserializable spec mapped onto the constructors.
//! Only the profiles the harness currently needs are supported; anything else is
//! a clear error rather than a silent fallback.

use std::collections::BTreeMap;
use std::sync::Arc;

use anyhow::{bail, Context, Result};
use serde::Deserialize;
use serde_json::Value;
use switchyard_libsy::{
    Algorithm, CapabilityJudgeConfig, ClassifyTrigger, DecisionJudgeConfig, HandoffNoteConfig,
    LlmClassifierConfig, LlmFallback, LlmTaskClassifier, Noop, PickerMode, Random, StageRouter,
    StageRouterConfig, TaskClassifierConfig,
};
use switchyard_protocol::ModelId;

/// A tenant-selectable profile: the algorithm and its config, no model pool.
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "algorithm", rename_all = "snake_case")]
pub enum Profile {
    Noop,
    Random {
        #[serde(default)]
        weights: Option<Vec<f64>>,
        #[serde(default)]
        seed: Option<u64>,
    },
    StageRouter {
        picker: String,
        #[serde(default = "default_confidence_threshold")]
        confidence_threshold: f64,
        #[serde(default)]
        recent_window: Option<usize>,
        #[serde(default)]
        capable_hold_turns: Option<u32>,
        #[serde(default)]
        escalation_note: Option<String>,
        #[serde(default)]
        deescalation_note: Option<String>,
        #[serde(default = "default_true")]
        only_on_wrong_signal_escalation: bool,
        #[serde(default)]
        capable_system_prompt: Option<String>,
        #[serde(default)]
        efficient_system_prompt: Option<String>,
        /// Omit for a fully local profile (zero host calls).
        #[serde(default)]
        llm_fallback: Option<JudgeSpec>,
    },
    LlmClassifier {
        #[serde(default = "default_mode")]
        mode: String,
        judge: JudgeSpec,
        #[serde(default = "default_true")]
        fail_open: bool,
        #[serde(default)]
        session_affinity: bool,
        #[serde(default)]
        message_hash_fallback: bool,
        #[serde(default)]
        recent_turn_window: Option<usize>,
    },
}

/// Judge configuration. Only the decision (JEV / System One) judge is supported.
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum JudgeSpec {
    Decision {
        cutoff: f64,
        candidates: BTreeMap<String, String>,
        #[serde(default)]
        evidence: Value,
        #[serde(default)]
        instructions: Option<Value>,
    },
}

impl JudgeSpec {
    fn decision_judge(&self) -> DecisionJudgeConfig {
        match self {
            JudgeSpec::Decision {
                cutoff,
                candidates,
                evidence,
                instructions,
            } => DecisionJudgeConfig {
                cutoff: *cutoff,
                instructions: instructions.clone(),
                candidates: candidates
                    .iter()
                    .map(|(label, model)| (label.clone(), ModelId::from(model.clone())))
                    .collect(),
                evidence: evidence.clone(),
            },
        }
    }
}

fn default_confidence_threshold() -> f64 {
    0.5
}

fn default_true() -> bool {
    true
}

fn default_mode() -> String {
    "capability".to_string()
}

fn parse_picker(picker: &str) -> Result<PickerMode> {
    Ok(match picker {
        "capable_first" => PickerMode::CapableFirst,
        "efficient_first" => PickerMode::EfficientFirst,
        other => bail!("unknown picker {other:?} (expected capable_first or efficient_first)"),
    })
}

fn classify_trigger(session_affinity: bool) -> ClassifyTrigger {
    if session_affinity {
        ClassifyTrigger::NewSession
    } else {
        ClassifyTrigger::EveryRequest
    }
}

fn task_config(
    judge: &JudgeSpec,
    fail_open: bool,
    session_affinity: bool,
    message_hash_fallback: bool,
    recent_turn_window: Option<usize>,
) -> TaskClassifierConfig {
    TaskClassifierConfig {
        judge: CapabilityJudgeConfig::Decision(judge.decision_judge()),
        fail_open,
        classify_trigger: classify_trigger(session_affinity),
        message_hash_fallback,
        recent_turn_window,
    }
}

/// Builds the algorithm for a profile.
pub fn build(profile: &Profile) -> Result<Arc<dyn Algorithm>> {
    match profile {
        Profile::Noop => Ok(Arc::new(Noop {})),
        Profile::Random { weights, seed } => Ok(Arc::new(
            Random::new(weights.clone(), *seed).context("building random profile")?,
        )),
        Profile::StageRouter {
            picker,
            confidence_threshold,
            recent_window,
            capable_hold_turns,
            escalation_note,
            deescalation_note,
            only_on_wrong_signal_escalation,
            capable_system_prompt,
            efficient_system_prompt,
            llm_fallback,
        } => {
            let mut config = StageRouterConfig::new(parse_picker(picker)?, *confidence_threshold);
            config.recent_window = *recent_window;
            if let Some(hold) = capable_hold_turns {
                config.capable_hold_turns = *hold;
            }
            config.handoff_notes = match (escalation_note, deescalation_note) {
                (Some(escalation), deescalation) => Some(HandoffNoteConfig::new(
                    escalation.clone(),
                    deescalation.clone(),
                    *only_on_wrong_signal_escalation,
                )),
                (None, Some(_)) => bail!("deescalation_note requires escalation_note"),
                (None, None) => None,
            };
            config.capable_system_prompt = capable_system_prompt.clone();
            config.efficient_system_prompt = efficient_system_prompt.clone();
            config.llm_fallback = llm_fallback.as_ref().map(|judge| LlmFallback {
                config: task_config(judge, true, false, false, *recent_window),
            });
            Ok(Arc::new(
                StageRouter::new(config).context("building stage_router profile")?,
            ))
        }
        Profile::LlmClassifier {
            mode,
            judge,
            fail_open,
            session_affinity,
            message_hash_fallback,
            recent_turn_window,
        } => {
            if mode != "capability" {
                bail!(
                    "unsupported llm_classifier mode {mode:?} (only `capability` with a `decision` judge is supported)"
                );
            }
            let config = task_config(
                judge,
                *fail_open,
                *session_affinity,
                *message_hash_fallback,
                *recent_turn_window,
            );
            Ok(Arc::new(
                LlmTaskClassifier::new(LlmClassifierConfig::Capability { config })
                    .context("building llm_classifier profile")?,
            ))
        }
    }
}
