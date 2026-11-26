"""Validate and compare complete IntelliGate Locust benchmark runs."""

import argparse
import csv
import json
import math
import re
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


CSV_COLUMNS = (
    "Name",
    "Request Count",
    "Failure Count",
    "Requests/s",
    "Median Response Time",
    "Average Response Time",
    "95%",
    "99%",
)
REQUIRED_ARTIFACTS = (
    "locust_stats.csv",
    "locust_stats_history.csv",
    "locust_failures.csv",
    "locust_exceptions.csv",
    "metadata.json",
    "prometheus-before.json",
    "prometheus-after.json",
)
METADATA_FIELDS = (
    "status",
    "strategy",
    "gateway_url",
    "prometheus_url",
    "users",
    "spawn_rate_users_per_second",
    "warmup_seconds",
    "warmup_elapsed_seconds",
    "measured_duration",
    "requested_measured_seconds",
    "measured_elapsed_seconds",
    "effective_gateway_config",
    "git_revision",
    "locust_version",
    "locustfile_sha256",
    "requirements_sha256",
    "runner_sha256",
    "workload",
)
CONFIG_FIELDS = (
    "backend_pools",
    "rate_limit",
    "retry",
    "circuit_breaker",
)


def gateway_duration_seconds(value: str) -> float:
    if not isinstance(value, str):
        raise ValueError("Gateway timeout settings must use positive Go durations")
    match = re.fullmatch(r"(?P<amount>\d+(?:\.\d+)?)(?P<unit>ns|us|µs|ms|s|m|h)", value or "")
    if not match:
        raise ValueError("Gateway timeout settings must use positive Go durations")
    factors = {"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "m": 60, "h": 3600}
    seconds = float(match.group("amount")) * factors[match.group("unit")]
    if not math.isfinite(seconds) or seconds <= 0:
        raise ValueError("Gateway timeout settings must use positive finite durations")
    return seconds


def benchmark_duration_seconds(value: str) -> float:
    if not isinstance(value, str):
        raise ValueError("Measured duration must use integer seconds, minutes, or hours")
    match = re.fullmatch(r"(?P<amount>\d+)(?P<unit>s|m|h)", value or "")
    if not match:
        raise ValueError("Measured duration must use integer seconds, minutes, or hours")
    factors = {"s": 1, "m": 60, "h": 3600}
    return float(match.group("amount")) * factors[match.group("unit")]


def read_json(path: Path) -> dict:
    try:
        with path.open(encoding="utf-8-sig") as handle:
            value = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise ValueError(f"Could not read valid JSON from {path.name}") from error
    if not isinstance(value, dict):
        raise ValueError(f"{path.name} must contain a JSON object")
    return value


def metric_map(snapshot: dict, name: str, labels_to_keep=("service", "backend_id", "attempt")) -> dict[str, float]:
    queries = snapshot.get("queries")
    if not isinstance(queries, dict):
        raise ValueError("Prometheus snapshot is missing its queries object")
    results = queries.get(name)
    if not isinstance(results, list):
        raise ValueError(f"Prometheus snapshot is missing the {name!r} query")
    values = {}
    for item in results:
        try:
            labels = item["metric"]
            if not isinstance(labels, dict):
                raise TypeError("metric labels must be an object")
            key = "/".join(labels.get(label, "") for label in labels_to_keep)
            value = float(item["value"][1])
        except (KeyError, TypeError, IndexError, ValueError) as error:
            raise ValueError(f"Prometheus snapshot contains malformed {name!r} data") from error
        if not math.isfinite(value):
            raise ValueError(f"Prometheus snapshot contains non-finite {name!r} data")
        if key in values:
            raise ValueError(f"Prometheus snapshot contains duplicate {name!r} series")
        values[key] = value
    return values


def validate_csv_artifact(path: Path) -> None:
    try:
        with path.open(newline="", encoding="utf-8-sig") as handle:
            reader = csv.reader(handle, strict=True)
            header = next(reader, None)
            if not header or any(not column.strip() for column in header):
                raise ValueError(f"{path.name} has no valid CSV header")
            width = len(header)
            for row in reader:
                if not row or all(not column.strip() for column in row):
                    continue
                if len(row) != width:
                    raise ValueError(f"{path.name} contains a malformed CSV row")
    except (OSError, csv.Error, UnicodeError) as error:
        raise ValueError(f"Could not read valid CSV from {path.name}") from error


def validate_health(metadata: dict, before: dict, after: dict) -> dict[str, float]:
    pools = metadata["effective_gateway_config"]["backend_pools"]
    if not isinstance(pools, dict):
        raise ValueError("Declared backend pools are malformed")
    before_health = metric_map(before, "backend_health", ("service", "backend_id"))
    after_health = metric_map(after, "backend_health", ("service", "backend_id"))
    if before_health != after_health:
        raise ValueError("Backend health changed during a run")

    expected = {}
    for service in ("products", "orders", "users"):
        pool = pools.get(service)
        urls = pool.get("urls") if isinstance(pool, dict) else None
        count = pool.get("instance_count") if isinstance(pool, dict) else None
        if (
            not isinstance(urls, list)
            or not urls
            or isinstance(count, bool)
            or not isinstance(count, int)
            or count != len(urls)
            or not all(isinstance(url, str) for url in urls)
        ):
            raise ValueError(f"Declared {service} backend pool is incomplete")
        for url in urls:
            parsed = urlsplit(url)
            if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
                raise ValueError(f"Declared {service} backend URL is malformed or contains sensitive URL fields")
        for index in range(1, count + 1):
            expected[f"{service}/{service}-{index}"] = 1.0
    if before_health != expected:
        raise ValueError(
            "Backend health metric IDs/counts do not match the declared pools; health metrics do not establish URL equality"
        )
    return before_health


def read_locust_stats(path: Path) -> dict:
    try:
        with path.open(newline="", encoding="utf-8-sig") as handle:
            reader = csv.DictReader(handle, strict=True)
            if reader.fieldnames is None or any(column not in reader.fieldnames for column in CSV_COLUMNS):
                raise ValueError("Locust statistics CSV is missing required columns")
            rows = [
                row
                for row in reader
                if any(value is not None and str(value).strip() for value in row.values())
            ]
    except (OSError, csv.Error) as error:
        raise ValueError("Could not read the Locust statistics CSV") from error

    aggregate = next(
        (
            row
            for row in rows
            if isinstance(row.get("Name"), str) and row["Name"].strip() == "Aggregated"
        ),
        None,
    )
    if aggregate is None:
        raise ValueError("Locust statistics CSV has no Aggregated row")

    def number(column: str) -> float:
        try:
            value = float(aggregate[column])
        except (KeyError, TypeError, ValueError) as error:
            raise ValueError(f"Locust statistics CSV has an invalid {column!r} value") from error
        if not math.isfinite(value) or value < 0:
            raise ValueError(f"Locust statistics CSV has an invalid {column!r} value")
        return value

    requests = number("Request Count")
    failures = number("Failure Count")
    if requests <= 0 or failures > requests:
        raise ValueError("Locust statistics CSV has no requests or an invalid failure count")
    return {
        "requests": int(requests),
        "failures": int(failures),
        "failure_rate_percent": failures / requests * 100,
        "throughput_requests_per_second": number("Requests/s"),
        "average_latency_ms": number("Average Response Time"),
        "median_latency_ms": number("Median Response Time"),
        "p95_latency_ms": number("95%"),
        "p99_latency_ms": number("99%"),
    }


def selection_delta(before: dict, after: dict) -> dict[str, int]:
    previous = metric_map(before, "selections")
    current = metric_map(after, "selections")
    deltas = {}
    for key in sorted(set(previous) | set(current)):
        if previous.get(key, 0.0) < 0 or current.get(key, 0.0) < 0:
            raise ValueError(f"Backend selection counter is invalid for {key}")
        delta = current.get(key, 0.0) - previous.get(key, 0.0)
        if delta < 0:
            raise ValueError(f"Backend selection counter reset during the run for {key}")
        rounded = round(delta)
        if not math.isclose(delta, rounded, abs_tol=1e-6):
            raise ValueError(f"Backend selection delta is not an integer for {key}")
        if rounded:
            deltas[key] = int(rounded)
    if not deltas:
        raise ValueError("No backend selection deltas were recorded")
    return deltas


def load_run(directory: Path) -> dict:
    if not directory.is_dir():
        raise ValueError(f"Run directory does not exist: {directory}")
    paths = {name: directory / name for name in REQUIRED_ARTIFACTS}
    for name, path in paths.items():
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError(f"Run is incomplete: missing or empty {name}")
        if name.endswith(".csv"):
            validate_csv_artifact(path)

    metadata = read_json(paths["metadata.json"])
    missing = [field for field in METADATA_FIELDS if field not in metadata]
    if missing:
        raise ValueError(f"Run metadata is incomplete: {', '.join(missing)}")
    if metadata["status"] != "completed":
        raise ValueError("Run metadata does not mark the benchmark completed")
    if isinstance(metadata["users"], bool) or not isinstance(metadata["users"], int) or metadata["users"] <= 0:
        raise ValueError("Run metadata has an invalid user count")
    if (
        isinstance(metadata["spawn_rate_users_per_second"], bool)
        or not isinstance(metadata["spawn_rate_users_per_second"], (int, float))
        or not math.isfinite(metadata["spawn_rate_users_per_second"])
        or metadata["spawn_rate_users_per_second"] <= 0
    ):
        raise ValueError("Run metadata has an invalid spawn rate")
    if isinstance(metadata["warmup_seconds"], bool) or not isinstance(metadata["warmup_seconds"], int) or metadata["warmup_seconds"] <= 0:
        raise ValueError("Run metadata has an invalid warm-up duration")
    if (
        isinstance(metadata["requested_measured_seconds"], bool)
        or not isinstance(metadata["requested_measured_seconds"], (int, float))
        or not math.isfinite(metadata["requested_measured_seconds"])
        or metadata["requested_measured_seconds"] < 15
    ):
        raise ValueError("Run metadata has an invalid measured duration")
    if not math.isclose(
        benchmark_duration_seconds(metadata["measured_duration"]),
        float(metadata["requested_measured_seconds"]),
        abs_tol=1e-6,
    ):
        raise ValueError("Measured duration label and numeric duration do not agree")
    if not isinstance(metadata["effective_gateway_config"], dict):
        raise ValueError("Run metadata is missing effective gateway configuration")
    for field in ("gateway_url", "prometheus_url"):
        value = metadata[field]
        parsed = urlsplit(value) if isinstance(value, str) else None
        if (
            parsed is None
            or parsed.scheme not in ("http", "https")
            or not parsed.hostname
            or parsed.username
            or parsed.password
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError(f"Run metadata has an invalid or sensitive {field}")
    if metadata["strategy"] not in ("round_robin", "adaptive"):
        raise ValueError("Run metadata has an unsupported routing strategy")
    if not isinstance(metadata["git_revision"], str) or not metadata["git_revision"].strip():
        raise ValueError("Run metadata has no Git revision")
    if not isinstance(metadata["locust_version"], str) or not metadata["locust_version"].strip():
        raise ValueError("Run metadata has no Locust version")
    if not isinstance(metadata["workload"], str) or not metadata["workload"].strip():
        raise ValueError("Run metadata has no workload description")
    for field in ("locustfile_sha256", "requirements_sha256", "runner_sha256"):
        if not isinstance(metadata[field], str) or not re.fullmatch(r"[0-9a-f]{64}", metadata[field]):
            raise ValueError(f"Run metadata has an invalid {field}")
    config = metadata["effective_gateway_config"]
    missing_config = [field for field in CONFIG_FIELDS if field not in config]
    if missing_config:
        raise ValueError(f"Effective gateway configuration is incomplete: {', '.join(missing_config)}")
    if any(not isinstance(config[field], dict) for field in CONFIG_FIELDS):
        raise ValueError("Effective gateway configuration sections must be JSON objects")
    rate_limit = config["rate_limit"]
    if rate_limit.get("enabled") is not True:
        raise ValueError("Benchmark comparison requires rate limiting to remain enabled")
    for key in ("rate_tokens_per_second", "burst_tokens"):
        value = rate_limit.get(key)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or not 0 < value <= 1000:
            raise ValueError(f"Effective rate-limit {key} is invalid or outside the supported benchmark range")
    if rate_limit.get("store") not in ("memory", "redis") or rate_limit.get("failure_mode") not in ("closed", "open"):
        raise ValueError("Effective rate-limit store or failure mode is invalid")
    redis_address = rate_limit.get("redis_address")
    parsed_redis = urlsplit(f"redis://{redis_address}") if isinstance(redis_address, str) else None
    if (
        parsed_redis is None
        or not parsed_redis.hostname
        or parsed_redis.username
        or parsed_redis.password
        or parsed_redis.query
        or parsed_redis.fragment
        or parsed_redis.path not in ("", "/")
        or parsed_redis.port is None
        or not 1 <= parsed_redis.port <= 65535
    ):
        raise ValueError("Effective Redis address is malformed or contains sensitive URL fields")
    redis_db = rate_limit.get("redis_db")
    if isinstance(redis_db, bool) or not isinstance(redis_db, int) or redis_db < 0:
        raise ValueError("Effective Redis database number is invalid")
    retry = config["retry"]
    if retry.get("max_retries") != 2 or retry.get("backoffs") != ["100ms", "200ms"] or retry.get("methods") != ["GET", "HEAD"]:
        raise ValueError("Retry settings do not match the gateway's supported fixed retry policy")
    if retry.get("upstream_timeout") != config["upstream_timeout"]:
        raise ValueError("Retry and gateway upstream timeout settings do not match")
    gateway_duration_seconds(config.get("upstream_timeout"))
    if retry.get("retry_budget") != "3 x upstream timeout + 300ms":
        raise ValueError("Retry budget does not match the gateway's supported retry policy")
    for key in ("failure_threshold", "half_open_max_requests", "half_open_success_threshold"):
        if not isinstance(config["circuit_breaker"].get(key), int) or config["circuit_breaker"][key] <= 0:
            raise ValueError(f"Circuit-breaker {key} must be a positive integer")
    gateway_duration_seconds(config["circuit_breaker"].get("open_cooldown"))

    elapsed_values = {}
    for field in ("warmup_elapsed_seconds", "measured_elapsed_seconds"):
        value = metadata[field]
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
            raise ValueError(f"Run metadata has an invalid {field}")
        elapsed_values[field] = float(value)
    warmup_elapsed = elapsed_values["warmup_elapsed_seconds"]
    measured_elapsed = elapsed_values["measured_elapsed_seconds"]
    if warmup_elapsed < metadata["warmup_seconds"] - 2:
        raise ValueError("Warm-up did not complete")
    if not math.isfinite(measured_elapsed) or measured_elapsed < metadata["requested_measured_seconds"] - 2:
        raise ValueError("Measured window ended early")

    stats = read_locust_stats(paths["locust_stats.csv"])
    before = read_json(paths["prometheus-before.json"])
    after = read_json(paths["prometheus-after.json"])
    health = validate_health(metadata, before, after)
    rate_before = metric_map(before, "rate_limit_rejections", ("reason",))
    rate_after = metric_map(after, "rate_limit_rejections", ("reason",))
    rate_limit_rejections = {}
    for key in sorted(set(rate_before) | set(rate_after)):
        if rate_before.get(key, 0.0) < 0 or rate_after.get(key, 0.0) < 0:
            raise ValueError(f"Rate-limit rejection counter is invalid for {key}")
        delta = rate_after.get(key, 0.0) - rate_before.get(key, 0.0)
        if delta < 0 or not math.isclose(delta, round(delta), abs_tol=1e-6):
            raise ValueError(f"Rate-limit rejection counter reset or became invalid for {key}")
        if delta:
            rate_limit_rejections[key] = int(round(delta))
    before_start = metric_map(before, "gateway_process_start_time", ("instance",))
    after_start = metric_map(after, "gateway_process_start_time", ("instance",))
    if len(before_start) != 1 or before_start != after_start:
        raise ValueError("Gateway process restarted during the measured run")

    return {
        "strategy": metadata["strategy"],
        "run_directory": str(directory),
        **stats,
        "backend_selection_delta": selection_delta(before, after),
        "rate_limit_rejection_delta": rate_limit_rejections,
        "backend_health": health,
        "users": metadata["users"],
        "spawn_rate_users_per_second": metadata["spawn_rate_users_per_second"],
        "warmup_seconds": metadata["warmup_seconds"],
        "warmup_elapsed_seconds": warmup_elapsed,
        "measured_duration": metadata["measured_duration"],
        "requested_measured_seconds": metadata["requested_measured_seconds"],
        "measured_elapsed_seconds": measured_elapsed,
        "gateway_url": metadata["gateway_url"],
        "prometheus_url": metadata["prometheus_url"],
        "effective_gateway_config": config,
        "git_revision": metadata["git_revision"],
        "locust_version": metadata["locust_version"],
        "locustfile_sha256": metadata["locustfile_sha256"],
        "requirements_sha256": metadata["requirements_sha256"],
        "runner_sha256": metadata["runner_sha256"],
        "workload": metadata["workload"],
    }


def percent_change(baseline: float, compared: float):
    if baseline == 0:
        return None
    return (compared - baseline) / baseline * 100


def markdown_report(round_robin: dict, adaptive: dict) -> str:
    fields = (
        ("Throughput (requests/s)", "throughput_requests_per_second"),
        ("Average latency (ms)", "average_latency_ms"),
        ("Median latency (ms)", "median_latency_ms"),
        ("P95 latency (ms)", "p95_latency_ms"),
        ("P99 latency (ms)", "p99_latency_ms"),
        ("Failure rate (%)", "failure_rate_percent"),
        ("Requests", "requests"),
        ("Failures", "failures"),
    )
    lines = [
        "# Round Robin vs Adaptive benchmark comparison",
        "",
        "This report presents measurements only. It does not infer that either strategy is better.",
        "",
        "| Measure | Round Robin | Adaptive | Adaptive vs Round Robin |",
        "| --- | ---: | ---: | ---: |",
    ]
    for label, key in fields:
        baseline, compared = round_robin[key], adaptive[key]
        change = percent_change(float(baseline), float(compared))
        change_text = "n/a (baseline is zero)" if change is None else f"{change:+.2f}%"
        lines.append(f"| {label} | {baseline:.2f} | {compared:.2f} | {change_text} |")
    lines.extend(["", "## Backend selections during each measured window", ""])
    for run in (round_robin, adaptive):
        lines.extend([f"### {run['strategy']}", ""])
        lines.extend(
            f"- `{key}`: {value}"
            for key, value in run["backend_selection_delta"].items()
        )
        lines.append("")
    lines.extend(["## Rate-limit rejections during each measured window", ""])
    for run in (round_robin, adaptive):
        rejected = run["rate_limit_rejection_delta"]
        exhausted = rejected.get("exhausted", 0)
        store_errors = rejected.get("store_unavailable", 0)
        lines.append(f"- {run['strategy']}: {exhausted} quota rejections (HTTP 429 metric), {store_errors} store-unavailable rejections.")
    lines.extend(
        [
            "## Configuration and interpretation",
            "",
            f"- Warm-up: {round_robin['warmup_seconds']} seconds, excluded from measured Locust CSVs and selection deltas.",
            f"- Measured window: {round_robin['measured_duration']} for both runs.",
            "- The comparator verified declared backend URL lists and full benchmark configuration; backend health metrics alone do not prove URL lists match.",
            "- Rate limiting remained enabled. HTTP 429 responses count as request failures and should be reported when interpreting latency and throughput.",
            "- Locust measures end-to-end client latency; gateway EWMA measures upstream TTFB.",
        ]
    )
    return "\n".join(lines) + "\n"


def compare_runs(round_robin: dict, adaptive: dict) -> dict:
    if round_robin["strategy"] != "round_robin" or adaptive["strategy"] != "adaptive":
        raise ValueError("Provide a completed round_robin run and a completed adaptive run")
    comparable = (
        "users",
        "spawn_rate_users_per_second",
        "warmup_seconds",
        "measured_duration",
        "requested_measured_seconds",
        "gateway_url",
        "prometheus_url",
        "backend_health",
        "effective_gateway_config",
        "git_revision",
        "locust_version",
        "locustfile_sha256",
        "requirements_sha256",
        "runner_sha256",
        "workload",
    )
    differing = [field for field in comparable if round_robin[field] != adaptive[field]]
    if differing:
        raise ValueError(f"Runs are not comparable; differing settings: {', '.join(differing)}")
    measured_tolerance = max(2.0, round_robin["requested_measured_seconds"] * 0.01)
    warmup_tolerance = max(2.0, round_robin["warmup_seconds"] * 0.05)
    if abs(round_robin["measured_elapsed_seconds"] - adaptive["measured_elapsed_seconds"]) > measured_tolerance:
        raise ValueError("Runs are not comparable; actual measured windows differ beyond timing tolerance")
    if abs(round_robin["warmup_elapsed_seconds"] - adaptive["warmup_elapsed_seconds"]) > warmup_tolerance:
        raise ValueError("Runs are not comparable; actual warm-up windows differ beyond timing tolerance")

    return {
        "round_robin": round_robin,
        "adaptive": adaptive,
        "relative_change_percent": {
            key: percent_change(float(round_robin[key]), float(adaptive[key]))
            for key in (
                "throughput_requests_per_second",
                "average_latency_ms",
                "median_latency_ms",
                "p95_latency_ms",
                "p99_latency_ms",
                "failure_rate_percent",
            )
        },
        "interpretation": "Measurements are reported without claiming either routing strategy performs better.",
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--round-robin", required=True, type=Path, help="Round Robin run directory")
    parser.add_argument("--adaptive", required=True, type=Path, help="Adaptive run directory")
    parser.add_argument("--output-dir", type=Path, help="Defaults to a timestamped comparison directory under benchmark-results")
    arguments = parser.parse_args()

    round_robin = load_run(arguments.round_robin)
    adaptive = load_run(arguments.adaptive)
    report = compare_runs(round_robin, adaptive)
    if arguments.output_dir:
        output_dir = arguments.output_dir
        if output_dir.exists() and any(output_dir.iterdir()):
            raise ValueError("Comparison output directory already contains files; choose a new directory")
    else:
        output_parent = Path("benchmark-results/phase-8")
        comparison_name = "comparison-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        output_dir = output_parent / comparison_name
        suffix = 1
        while output_dir.exists():
            output_dir = output_parent / f"{comparison_name}-{suffix}"
            suffix += 1
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "comparison.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    (output_dir / "comparison.md").write_text(markdown_report(round_robin, adaptive), encoding="utf-8")
    print(output_dir / "comparison.md")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise SystemExit(f"Comparison failed: {error}") from error
