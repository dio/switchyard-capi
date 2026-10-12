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
    Algorithm, CapabilityJudgeConfig, ClassifyTrigger, CompositeRouter, CompositeRouterConfig,
    DecisionJudgeConfig, HandoffNoteConfig,
    LlmClassifierConfig, LlmFallback, LlmTaskClassifier, Noop, Passthrough, PickerMode, Random,
    StageRouter, StageRouterConfig, TaskClassifierConfig,
};
use switchyard_protocol::ModelId;

/// A tenant-selectable profile: the algorithm and its config, no model pool.
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "algorithm", rename_all = "snake_case")]
pub enum Profile {
    Noop,
    Passthrough,
    Random {
        #[serde(default)]
        weights: Option<Vec<f64>>,
        #[serde(default)]
        seed: Option<u64>,
    },
    StageRouter {
        #[serde(flatten)]
        stage: StageSpec,
        /// Omit for a fully local profile (zero host calls).
        #[serde(default)]
        llm_fallback: Option<JudgeSpec>,
    },
    Composite {
        /// The judge that picks the tier the stage router falls open to.
        judge: JudgeSpec,
        /// How often the judge re-decides, and whether an unkeyed request may retain.
        #[serde(default = "default_trigger")]
        classify_trigger: String,
        #[serde(default)]
        message_hash_fallback: bool,
        #[serde(flatten)]
        stage: StageSpec,
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

/// The stage router's configuration, shared with composite.
#[derive(Debug, Clone, Deserialize)]
pub struct StageSpec {
    pub picker: String,
    #[serde(default = "default_confidence_threshold")]
    pub confidence_threshold: f64,
    #[serde(default)]
    pub recent_window: Option<usize>,
    #[serde(default)]
    pub capable_hold_turns: Option<u32>,
    #[serde(default)]
    pub escalation_note: Option<String>,
    #[serde(default)]
    pub deescalation_note: Option<String>,
    #[serde(default = "default_true")]
    pub only_on_wrong_signal_escalation: bool,
    #[serde(default)]
    pub capable_system_prompt: Option<String>,
    #[serde(default)]
    pub efficient_system_prompt: Option<String>,
}

impl StageSpec {
    fn config(&self) -> Result<StageRouterConfig> {
        let mut config = StageRouterConfig::new(parse_picker(&self.picker)?, self.confidence_threshold);
        config.recent_window = self.recent_window;
        if let Some(hold) = self.capable_hold_turns {
            config.capable_hold_turns = hold;
        }
        config.handoff_notes = match (&self.escalation_note, &self.deescalation_note) {
            (Some(escalation), deescalation) => Some(HandoffNoteConfig::new(
                escalation.clone(),
                deescalation.clone(),
                self.only_on_wrong_signal_escalation,
            )),
            (None, Some(_)) => bail!("deescalation_note requires escalation_note"),
            (None, None) => None,
        };
        config.capable_system_prompt = self.capable_system_prompt.clone();
        config.efficient_system_prompt = self.efficient_system_prompt.clone();
        Ok(config)
    }
}

fn default_trigger() -> String {
    "new_session".to_string()
}

fn parse_trigger(trigger: &str) -> Result<ClassifyTrigger> {
    Ok(match trigger {
        "user_turn" => ClassifyTrigger::UserTurn,
        "new_session" => ClassifyTrigger::NewSession,
        "every_request" => ClassifyTrigger::EveryRequest,
        other => bail!("unknown classify_trigger {other:?}"),
    })
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
        Profile::Passthrough => Ok(Arc::new(Passthrough)),
        Profile::Random { weights, seed } => Ok(Arc::new(
            Random::new(weights.clone(), *seed).context("building random profile")?,
        )),
        Profile::StageRouter { stage, llm_fallback } => {
            let mut config = stage.config()?;
            config.llm_fallback = llm_fallback.as_ref().map(|judge| LlmFallback {
                config: task_config(judge, true, false, false, stage.recent_window),
            });
            Ok(Arc::new(
                StageRouter::new(config).context("building stage_router profile")?,
            ))
        }
        Profile::Composite {
            judge,
            classify_trigger,
            message_hash_fallback,
            stage,
        } => {
            // Built directly rather than through `task_config`, whose third parameter is
            // the standalone route's `session_affinity` bool — a different thing from the
            // trigger this router is configured with.
            let config = CompositeRouterConfig {
                judge: TaskClassifierConfig {
                    judge: CapabilityJudgeConfig::Decision(judge.decision_judge()),
                    fail_open: true,
                    classify_trigger: parse_trigger(classify_trigger)?,
                    message_hash_fallback: *message_hash_fallback,
                    recent_turn_window: stage.recent_window,
                },
                stage: stage.config()?,
            };
            Ok(Arc::new(
                CompositeRouter::new(config).context("building composite profile")?,
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
