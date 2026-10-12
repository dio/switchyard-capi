//! Drives one corpus case through `libsy`, serving each offloaded step from the
//! case's scripted decider responses.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

use anyhow::{anyhow, bail, Context, Result};
use futures::StreamExt;
use serde_json::Value;
use http::HeaderMap;
use switchyard_libsy::{Algorithm, LibsyError, RoutingOutcome, RuntimeModels, Step};
use switchyard_protocol::{
    text_request, AggLlmResponse, Category, DecisionResponse, LlmRequest, LlmResponse, Metadata,
    ModelId, Request, Response,
};
use switchyard_translation::{TranslationEngine, TranslationPolicy};

use crate::corpus::{Case, Scripted, TurnSpec};
use crate::result::TurnResult;

/// Runs every turn of a case against one algorithm instance, so session state and
/// affinity persist across turns exactly as they would in production.
pub async fn run_case(case: &Case) -> Result<Vec<TurnResult>> {
    let algorithm = crate::profile::build(&case.profile)?;
    let models = Arc::new(runtime_models(&case.models, &case.subagent_models)?);
    let turns = case.turn_specs();

    let mut results = Vec::with_capacity(turns.len());
    for (index, turn) in turns.iter().enumerate() {
        results.push(run_turn(case, index, algorithm.clone(), models.clone(), turn).await?);
    }
    Ok(results)
}

fn runtime_models(
    models: &BTreeMap<String, Vec<String>>,
    subagent: &Option<BTreeMap<String, Vec<String>>>,
) -> Result<RuntimeModels> {
    validate_pool(models)?;
    let mut runtime = RuntimeModels::new(categories(models)?);
    if let Some(subagent) = subagent {
        validate_pool(subagent)?;
        runtime = runtime.with_subagent(categories(subagent)?);
    }
    Ok(runtime)
}

/// Mirrors `switchyard-runner`'s check. Fall-through routers resolve the selected
/// target against `models.any` (`fall_through.rs`), so every routable target must
/// appear there too; a group outside it can never be served. The runner derives
/// this pool from target names, but a host handing libsy an explicit pool — as a
/// Go implementation would — must get it right, so it is validated here rather
/// than surfacing later as `TargetNotFound`.
fn validate_pool(models: &BTreeMap<String, Vec<String>>) -> Result<()> {
    let any = models.get("any").map(Vec::as_slice).unwrap_or(&[]);
    if any.is_empty() {
        bail!(
            "models.any must list every routable target: fall-through resolves the selected target against it"
        );
    }
    for (name, ids) in models {
        if name == "any" || name == "judge" {
            continue;
        }
        for id in ids {
            if !any.contains(id) {
                bail!("models.{name} lists {id:?}, which must also appear in models.any");
            }
        }
    }
    Ok(())
}

fn categories(models: &BTreeMap<String, Vec<String>>) -> Result<HashMap<Category, Vec<ModelId>>> {
    let mut by_category = HashMap::new();
    for (name, ids) in models {
        let category = name
            .parse::<Category>()
            .map_err(|error| anyhow!("invalid model category {name:?}: {error}"))?;
        by_category.insert(
            category,
            ids.iter().cloned().map(ModelId::from).collect::<Vec<_>>(),
        );
    }
    Ok(by_category)
}

fn headers_map(headers: &BTreeMap<String, String>) -> Result<HeaderMap> {
    let mut map = HeaderMap::new();
    for (name, value) in headers {
        map.insert(
            http::HeaderName::from_bytes(name.as_bytes())
                .with_context(|| format!("invalid header name {name:?}"))?,
            http::HeaderValue::from_str(value)
                .with_context(|| format!("invalid header value for {name:?}"))?,
        );
    }
    Ok(map)
}

/// Builds a turn's request: a decoded provider body when the case carries one,
/// otherwise a synthetic single-message request from the prompt.
///
/// A raw case exists to exercise the normalizer, so decoding it is the point. The
/// recorded `messages_digest` and `instructions` are then the normalizer's
/// fingerprint, and a port that decodes differently fails the diff — which a
/// synthetic request could never show, because there would be nothing to decode.
fn turn_request(turn: &TurnSpec) -> Result<LlmRequest> {
    let mut request = match &turn.raw_request {
        Some(body) => {
            let format = turn.wire_format.as_deref().unwrap_or("openai_chat");
            TranslationEngine::default()
                .decode_request(format, body, &TranslationPolicy::default())
                .with_context(|| format!("decoding a {format} raw_request"))?
                .request
        }
        None => text_request(turn.model.clone(), turn.prompt.clone().unwrap_or_default()),
    };
    if turn.stream == Some(true) {
        request.stream = true;
    }
    Ok(request)
}

async fn run_turn(
    case: &Case,
    index: usize,
    algorithm: Arc<dyn Algorithm>,
    models: Arc<RuntimeModels>,
    turn: &TurnSpec,
) -> Result<TurnResult> {
    let llm_request = turn_request(turn)?;
    let metadata = if turn.headers.is_empty() {
        None
    } else {
        Some(Metadata::from_headers(&headers_map(&turn.headers)?))
    };
    let request = Request {
        llm_request,
        raw_request: None,
        metadata,
    };

    let scripted = turn.decider_responses.clone().unwrap_or_default().by_index;

    let stream = algorithm.run_stream(request, models);
    tokio::pin!(stream);

    // What the judge was shown, from the first decision call of the turn.
    let mut judge_task: Option<Value> = None;
    let mut steps: Vec<String> = Vec::new();
    let mut call_index = 0usize;
    let mut outcome: Option<RoutingOutcome> = None;
    let mut error: Option<String> = None;

    loop {
        match stream.next().await {
            // The stream is exhausted without a terminal step: treat as an error.
            None => {
                if error.is_none() && outcome.is_none() {
                    error = Some("run ended without a Done step".to_string());
                }
                break;
            }
            Some(Err(err)) => {
                steps.push("Error".to_string());
                error = Some(err.to_string());
                break;
            }
            Some(Ok(step)) => match step {
                Step::CallModel(call) => {
                    steps.push("CallModel".to_string());
                    let scripted = scripted.get(call_index).ok_or_else(|| {
                        anyhow!(
                            "unexpected CallModel at index {call_index}; add a decider_responses.by_index entry"
                        )
                    })?;
                    call_index += 1;
                    match scripted {
                        Scripted::Model { response } => {
                            let aggregate: AggLlmResponse =
                                serde_json::from_value(response.clone())
                                    .context("parsing scripted model response")?;
                            call.respond(Ok(Response {
                                llm_response: LlmResponse::Agg(aggregate),
                                metadata: None,
                                upstream_headers: HeaderMap::new(),
                            }))?;
                        }
                        Scripted::ModelError { message } => {
                            call.respond(Err(LibsyError::AlgorithmError {
                                message: message.clone(),
                            }))?;
                        }
                        other => bail!("scripted step {other:?} cannot serve a model call"),
                    }
                }
                Step::CallDecision(call) => {
                    steps.push("CallDecision".to_string());
                    if judge_task.is_none() {
                        judge_task = call.request.context.get("task").cloned();
                    }
                    let scripted = scripted.get(call_index).ok_or_else(|| {
                        anyhow!(
                            "unexpected CallDecision at index {call_index}; add a decider_responses.by_index entry"
                        )
                    })?;
                    call_index += 1;
                    match scripted {
                        Scripted::Decision { response } => {
                            let response: DecisionResponse =
                                serde_json::from_value(response.clone())
                                    .context("parsing scripted decision response")?;
                            call.respond(Ok(response))?;
                        }
                        Scripted::DecisionError { message } => {
                            call.respond(Err(LibsyError::AlgorithmError {
                                message: message.clone(),
                            }))?;
                        }
                        other => bail!("scripted step {other:?} cannot serve a decision call"),
                    }
                }
                Step::Done(done) => {
                    steps.push("Done".to_string());
                    outcome = Some(*done);
                }
            },
        }
    }

    // Anything scripted but never consumed means the route took a different path
    // than the case anticipated. Recorded, not fatal.
    let unconsumed = scripted.len().saturating_sub(call_index);

    let mut result = TurnResult::from_run(case, index, steps, outcome.as_ref(), unconsumed, error);
    result.judge_task = judge_task;
    Ok(result)
}
