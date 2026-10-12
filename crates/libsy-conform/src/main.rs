//! Conformance harness for Switchyard `libsy` routing decisions.
//!
//! Runs a JSONL corpus through `libsy` (the oracle) and either records the
//! results as golden output or verifies a fresh run against it. The recorded
//! JSON defines the schema every other implementation must match.

mod corpus;
mod profile;
mod result;
mod runner;

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde_json::Value;

use result::TurnResult;

const USAGE: &str = "\
libsy-conform — differential conformance harness for libsy

USAGE:
    libsy-conform [OPTIONS]

OPTIONS:
    --corpus <DIR>   corpus directory of *.jsonl suites   [default: ../libsygo/testdata/corpus]
    --golden <DIR>   recorded results directory           [default: ../libsygo/testdata/golden]
    --mode <MODE>    record | verify | bless | impl       [default: verify]
    --impl <PATH>    impl runner binary                   [default: .bin/libsy-conform-impl]
    --tier <N>       only cases with this tier
    --case <GLOB>    only case_ids matching this glob (single '*')

MODES:
    record   run the oracle and write golden results
    verify   run the oracle and diff against golden (determinism + drift gate)
    bless    alias for record
    impl     run an independent implementation and diff it against golden

Exit code is non-zero on any diff, missing/extra case, or error.";

struct Args {
    corpus: PathBuf,
    golden: PathBuf,
    mode: Mode,
    impl_bin: PathBuf,
    tier: Option<u8>,
    case: Option<String>,
}

enum Mode {
    Record,
    Verify,
    Bless,
    Impl,
}

fn main() -> Result<()> {
    let args = parse_args()?;
    let suites = collect_suites(&args.corpus, args.tier, args.case.as_deref())?;
    if suites.is_empty() {
        bail!("no corpus cases found in {}", args.corpus.display());
    }

    let runtime = tokio::runtime::Runtime::new().context("starting tokio runtime")?;

    match args.mode {
        Mode::Record | Mode::Bless => {
            let results = runtime.block_on(run_suites(&suites))?;
            write_golden(&args.golden, &suites, &results)?;
            println!(
                "recorded {} turns to {}",
                results.len(),
                args.golden.display()
            );
            Ok(())
        }
        Mode::Verify => {
            let actual = runtime.block_on(run_suites(&suites))?;
            let actual = flatten(&actual);
            let golden = read_golden(&args.golden)?;
            let problems = diff(&golden, &actual);
            if problems.is_empty() {
                println!("ok: {} turns match golden", actual.len());
                Ok(())
            } else {
                for problem in &problems {
                    eprintln!("diff: {problem}");
                }
                bail!("{} difference(s) against golden", problems.len())
            }
        }
        Mode::Impl => {
            // Diff like-for-like: the impl honours --tier/--case, so the golden
            // must be narrowed to the same case selection.
            let allowed: BTreeSet<&str> = suites
                .iter()
                .flat_map(|(_, cases)| cases.iter().map(|case| case.case_id.as_str()))
                .collect();
            let (problems, count) = run_impl(&args, &allowed)?;
            if problems.is_empty() {
                println!("ok: impl matches golden across {count} turns");
                Ok(())
            } else {
                for problem in &problems {
                    eprintln!("diff: {problem}");
                }
                bail!("{} difference(s) between impl and golden", problems.len())
            }
        }
    }
}

/// Runs the independent implementation over the corpus and diffs its results
/// against the recorded golden.
fn run_impl(args: &Args, allowed: &BTreeSet<&str>) -> Result<(Vec<String>, usize)> {
    let tmp = std::env::temp_dir().join(format!("libsy-conform-impl-{}.jsonl", std::process::id()));
    let _ = std::fs::remove_file(&tmp);

    let mut command = std::process::Command::new(&args.impl_bin);
    command
        .arg("--corpus")
        .arg(&args.corpus)
        .arg("--out")
        .arg(&tmp);
    if let Some(tier) = args.tier {
        command.arg("--tier").arg(tier.to_string());
    }
    if let Some(case) = &args.case {
        command.arg("--case").arg(case);
    }
    let status = command
        .status()
        .with_context(|| format!("running impl binary {}", args.impl_bin.display()))?;
    if !status.success() {
        bail!(
            "impl binary {} exited with {status}",
            args.impl_bin.display()
        );
    }

    let actual = read_results_file(&tmp)?;
    let _ = std::fs::remove_file(&tmp);
    let golden: Vec<TurnResult> = read_golden(&args.golden)?
        .into_iter()
        .filter(|turn| allowed.contains(turn.case_id.as_str()))
        .collect();
    Ok((diff(&golden, &actual), actual.len()))
}

/// Locates the corpus, which is canonical in libsygo.
///
/// It lives there because it is that library's conformance spec: libsygo replays
/// it in its own CI with no Rust toolchain and no network, which is the whole
/// reason the port exists. The harness only reads it, so the recording side stays
/// here — where the oracle, the pin and the drift job already are.
///
/// `LIBSYGO_TESTDATA` overrides the location for the case where the two
/// repositories are not checked out side by side.
fn libsygo_testdata() -> PathBuf {
    std::env::var("LIBSYGO_TESTDATA")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("../libsygo/testdata"))
}

fn parse_args() -> Result<Args> {
    let testdata = libsygo_testdata();
    let mut corpus = testdata.join("corpus");
    let mut golden = testdata.join("golden");
    let mut impl_bin = PathBuf::from(".bin/libsy-conform-impl");
    let mut mode = Mode::Verify;
    let mut tier = None;
    let mut case = None;

    let mut argv = std::env::args().skip(1);
    while let Some(arg) = argv.next() {
        match arg.as_str() {
            "--corpus" => corpus = PathBuf::from(next(&mut argv, "--corpus")?),
            "--golden" => golden = PathBuf::from(next(&mut argv, "--golden")?),
            "--impl" => impl_bin = PathBuf::from(next(&mut argv, "--impl")?),
            "--mode" => {
                mode = match next(&mut argv, "--mode")?.as_str() {
                    "record" => Mode::Record,
                    "verify" => Mode::Verify,
                    "bless" => Mode::Bless,
                    "impl" => Mode::Impl,
                    other => bail!("unknown --mode {other:?} (record|verify|bless|impl)"),
                }
            }
            "--tier" => {
                tier = Some(
                    next(&mut argv, "--tier")?
                        .parse()
                        .context("--tier must be a number")?,
                )
            }
            "--case" => case = Some(next(&mut argv, "--case")?),
            "-h" | "--help" => {
                println!("{USAGE}");
                std::process::exit(0);
            }
            other => bail!("unknown argument {other:?}\n\n{USAGE}"),
        }
    }

    Ok(Args {
        corpus,
        golden,
        mode,
        impl_bin,
        tier,
        case,
    })
}

fn next(argv: &mut impl Iterator<Item = String>, flag: &str) -> Result<String> {
    argv.next().with_context(|| format!("{flag} needs a value"))
}

type Suite = (String, Vec<corpus::Case>);
type Results = BTreeMap<String, Vec<TurnResult>>;

fn collect_suites(dir: &Path, tier: Option<u8>, case_glob: Option<&str>) -> Result<Vec<Suite>> {
    let mut suites = Vec::new();
    let mut entries: Vec<_> = std::fs::read_dir(dir)
        .with_context(|| format!("reading corpus directory {}", dir.display()))?
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().is_some_and(|ext| ext == "jsonl"))
        .collect();
    entries.sort();

    for path in entries {
        let mut cases = corpus::load_suite(&path)?;
        if let Some(tier) = tier {
            cases.retain(|case| case.tier == Some(tier));
        }
        if let Some(glob) = case_glob {
            cases.retain(|case| glob_match(glob, &case.case_id));
        }
        if cases.is_empty() {
            continue;
        }
        let name = path
            .file_stem()
            .map(|stem| stem.to_string_lossy().into_owned())
            .unwrap_or_default();
        suites.push((name, cases));
    }
    Ok(suites)
}

async fn run_suites(suites: &[Suite]) -> Result<Results> {
    let mut results: Results = BTreeMap::new();
    for (name, cases) in suites {
        let mut turns = Vec::new();
        for case in cases {
            // A case that fails before it can route — an undecodable request, a
            // profile libsy refuses — is a *result*, not a harness failure. The
            // oracle is saying what libsy does with that input, and the port must
            // say the same. Aborting here would make such a case unauthorable, and
            // "rejects invalid input" is exactly the behaviour a port gets wrong by
            // silently coercing.
            match runner::run_case(case).await {
                Ok(case_turns) => turns.extend(case_turns),
                Err(error) => {
                    let rejected = TurnResult::rejected(case, &error);
                    eprintln!("{}: rejected: {error:#}", case.case_id);
                    turns.push(rejected);
                }
            }
        }
        results.insert(name.clone(), turns);
    }
    Ok(results)
}

fn write_golden(dir: &Path, suites: &[Suite], results: &Results) -> Result<()> {
    std::fs::create_dir_all(dir).with_context(|| format!("creating {}", dir.display()))?;
    for (name, _) in suites {
        let Some(turns) = results.get(name) else {
            continue;
        };
        let path = dir.join(format!("{name}.jsonl"));
        let mut body = String::new();
        for turn in turns {
            body.push_str(&serde_json::to_string(turn)?);
            body.push('\n');
        }
        std::fs::write(&path, body).with_context(|| format!("writing {}", path.display()))?;
    }
    Ok(())
}

fn read_golden(dir: &Path) -> Result<Vec<TurnResult>> {
    let mut turns = Vec::new();
    for entry in std::fs::read_dir(dir)
        .with_context(|| format!("reading golden directory {}", dir.display()))?
    {
        let path = entry?.path();
        if path.extension().is_none_or(|ext| ext != "jsonl") {
            continue;
        }
        turns.extend(read_results_file(&path)?);
    }
    Ok(turns)
}

fn read_results_file(path: &Path) -> Result<Vec<TurnResult>> {
    let text =
        std::fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
    let mut turns = Vec::new();
    for (line, raw) in text.lines().enumerate() {
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        let turn: TurnResult = serde_json::from_str(trimmed)
            .with_context(|| format!("{}:{}: parsing result", path.display(), line + 1))?;
        turns.push(turn);
    }
    Ok(turns)
}

fn flatten(results: &Results) -> Vec<TurnResult> {
    results.values().flatten().cloned().collect()
}

fn index(turns: &[TurnResult]) -> BTreeMap<(String, usize), &TurnResult> {
    turns
        .iter()
        .map(|turn| ((turn.case_id.clone(), turn.turn), turn))
        .collect()
}

fn diff(golden: &[TurnResult], actual: &[TurnResult]) -> Vec<String> {
    let expected = index(golden);
    let observed = index(actual);

    let mut problems = Vec::new();
    for key in expected.keys() {
        if !observed.contains_key(key) {
            problems.push(format!("missing result {} turn {}", key.0, key.1));
        }
    }
    for key in observed.keys() {
        if !expected.contains_key(key) {
            problems.push(format!("extra result {} turn {}", key.0, key.1));
        }
    }
    for (key, golden_turn) in &expected {
        let Some(actual_turn) = observed.get(key) else {
            continue;
        };
        let golden_json = serde_json::to_value(golden_turn).ok();
        let actual_json = serde_json::to_value(actual_turn).ok();
        if golden_json == actual_json {
            continue;
        }
        match (golden_json, actual_json) {
            (Some(a), Some(b)) => match first_difference(&a, &b, String::new()) {
                Some((path, expected, found)) => problems.push(format!(
                    "{} turn {}: {path} golden={expected} actual={found}",
                    key.0, key.1
                )),
                None => problems.push(format!("{} turn {}: differs", key.0, key.1)),
            },
            _ => problems.push(format!("{} turn {}: differs", key.0, key.1)),
        }
    }
    problems
}

fn first_difference(a: &Value, b: &Value, path: String) -> Option<(String, Value, Value)> {
    match (a, b) {
        (Value::Object(expected), Value::Object(found)) => {
            for (key, expected_value) in expected {
                match found.get(key) {
                    Some(found_value) => {
                        if let Some(diff) =
                            first_difference(expected_value, found_value, format!("{path}/{key}"))
                        {
                            return Some(diff);
                        }
                    }
                    None => {
                        return Some((format!("{path}/{key}"), expected_value.clone(), Value::Null))
                    }
                }
            }
            for (key, found_value) in found {
                if !expected.contains_key(key) {
                    return Some((format!("{path}/{key}"), Value::Null, found_value.clone()));
                }
            }
            None
        }
        (Value::Array(expected), Value::Array(found)) => {
            if expected.len() != found.len() {
                return Some((
                    format!("{path}/len"),
                    Value::from(expected.len()),
                    Value::from(found.len()),
                ));
            }
            for index in 0..expected.len() {
                if let Some(diff) =
                    first_difference(&expected[index], &found[index], format!("{path}/{index}"))
                {
                    return Some(diff);
                }
            }
            None
        }
        _ if a != b => Some((path, a.clone(), b.clone())),
        _ => None,
    }
}

fn glob_match(pattern: &str, text: &str) -> bool {
    match pattern.split_once('*') {
        Some((prefix, suffix)) => {
            text.starts_with(prefix)
                && text.ends_with(suffix)
                && text.len() >= prefix.len() + suffix.len()
        }
        None => pattern == text,
    }
}
