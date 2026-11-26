import csv
import json
import tempfile
import unittest
from pathlib import Path

from compare_results import compare_runs, load_run, read_locust_stats, validate_csv_artifact


def gateway_config():
    return {
        "source": "test operator config",
        "backend_pools": {
            service: {
                "urls": [f"http://localhost:{port}", f"http://localhost:{port + 10}"],
                "instance_count": 2,
            }
            for service, port in (("products", 8001), ("orders", 8002), ("users", 8003))
        },
        "rate_limit": {
            "enabled": True,
            "rate_tokens_per_second": 1000.0,
            "burst_tokens": 1000.0,
            "store": "memory",
            "redis_address": "localhost:6379",
            "redis_db": 0,
            "failure_mode": "closed",
        },
        "retry": {
            "max_retries": 2,
            "backoffs": ["100ms", "200ms"],
            "methods": ["GET", "HEAD"],
            "retry_budget": "3 x upstream timeout + 300ms",
            "upstream_timeout": "5s",
        },
        "upstream_timeout": "5s",
        "circuit_breaker": {
            "failure_threshold": 5,
            "open_cooldown": "10s",
            "half_open_max_requests": 1,
            "half_open_success_threshold": 1,
        },
    }


def metric(labels, value):
    return {"metric": labels, "value": [1000, str(value)]}


class ComparisonTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.rr_dir = self.root / "round_robin"
        self.adaptive_dir = self.root / "adaptive"
        self.write_run(self.rr_dir, "round_robin", 100, selection_before=10, selection_after=30)
        self.write_run(self.adaptive_dir, "adaptive", 110, selection_before=50, selection_after=75)

    def tearDown(self):
        self.temporary.cleanup()

    def test_csv_validator_accepts_blank_separator_rows(self):
        path = self.root / "locust_stats.csv"
        path.write_text(
            "Type,Name,Request Count,Failure Count,Median Response Time,Average Response Time,"
            "Min Response Time,Max Response Time,Average Content Size,Requests/s,Failures/s,"
            "50%,95%,99%,100%\n"
            "GET,GET /api/v1/products,2,0,8,10,7,12,100,2,0,8,12,12,12\n"
            "\n   \n, \t\n"
            ",Aggregated,3,0,8,10,7,12,100,3,0,8,12,12,12\n",
            encoding="utf-8",
        )
        validate_csv_artifact(path)
        self.assertEqual(read_locust_stats(path)["requests"], 3)

    def test_csv_validator_accepts_rows_without_separators(self):
        path = self.root / "no_blank_rows.csv"
        path.write_text("name,count\nproducts,2\norders,3\n", encoding="utf-8")
        validate_csv_artifact(path)

    def test_csv_validator_rejects_nonblank_wrong_column_count(self):
        path = self.root / "wrong_width.csv"
        path.write_text("name,count\nproducts,2\norders\n", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "malformed CSV row"):
            validate_csv_artifact(path)

    def test_csv_validator_rejects_malformed_quoting(self):
        path = self.root / "bad_quoting.csv"
        path.write_text('name,count\n"products,2\n', encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "valid CSV"):
            validate_csv_artifact(path)

    def test_header_only_locust_stats_remains_incomplete(self):
        path = self.root / "header_only_stats.csv"
        path.write_text(",".join((
            "Name", "Request Count", "Failure Count", "Requests/s",
            "Median Response Time", "Average Response Time", "95%", "99%",
        )) + "\n", encoding="utf-8")
        validate_csv_artifact(path)
        with self.assertRaisesRegex(ValueError, "no Aggregated row"):
            read_locust_stats(path)

    def write_run(self, directory, strategy, request_count, *, config=None, users=20,
                  spawn_rate=5, duration="5m", warmup=30, status="completed",
                  gateway_url="http://127.0.0.1:8080", prometheus_url="http://127.0.0.1:9090",
                  selection_before=10, selection_after=15, health_after=None,
                  malformed_csv=False, measured_elapsed=None, warmup_elapsed=None):
        directory.mkdir(parents=True)
        duration_factor = {"s": 1, "m": 60, "h": 3600}[duration[-1]]
        measured_seconds = int(duration[:-1]) * duration_factor
        metadata = {
            "status": status,
            "strategy": strategy,
            "gateway_url": gateway_url,
            "prometheus_url": prometheus_url,
            "users": users,
            "spawn_rate_users_per_second": spawn_rate,
            "warmup_seconds": warmup,
            "warmup_elapsed_seconds": warmup + 0.1 if warmup_elapsed is None else warmup_elapsed,
            "measured_duration": duration,
            "requested_measured_seconds": measured_seconds,
            "measured_elapsed_seconds": measured_seconds + 0.1 if measured_elapsed is None else measured_elapsed,
            "effective_gateway_config": config or gateway_config(),
            "git_revision": "test-revision",
            "locust_version": "2.47.0",
            "locustfile_sha256": "a" * 64,
            "requirements_sha256": "b" * 64,
            "runner_sha256": "c" * 64,
            "workload": "equal-weight Product, Order, and User GET workload",
        }
        (directory / "metadata.json").write_text(json.dumps(metadata), encoding="utf-8")

        fields = [
            "Type", "Name", "Request Count", "Failure Count", "Median Response Time",
            "Average Response Time", "Min Response Time", "Max Response Time",
            "Average Content Size", "Requests/s", "Failures/s", "50%", "95%", "99%", "100%",
        ]
        if malformed_csv:
            fields = ["Name", "Request Count"]
        with (directory / "locust_stats.csv").open("w", newline="", encoding="utf-8") as handle:
            writer = csv.DictWriter(handle, fieldnames=fields)
            writer.writeheader()
            aggregate = {
                "Type": "", "Name": "Aggregated", "Request Count": request_count,
                "Failure Count": 12, "Median Response Time": 8,
                "Average Response Time": 10, "Min Response Time": 1,
                "Max Response Time": 50, "Average Content Size": 100,
                "Requests/s": 20, "Failures/s": 0.4, "50%": 8,
                "95%": 25, "99%": 40, "100%": 50,
            }
            writer.writerow({field: aggregate.get(field, "") for field in fields})
        for name in ("locust_stats_history.csv", "locust_failures.csv", "locust_exceptions.csv"):
            (directory / name).write_text("header\n", encoding="utf-8")

        health_before = [
            metric({"service": service, "backend_id": f"{service}-{index}"}, 1)
            for service in ("products", "orders", "users")
            for index in (1, 2)
        ]
        health_after = health_before if health_after is None else health_after
        selections_before = [metric({"service": "products", "backend_id": "products-1", "attempt": "initial"}, selection_before)]
        selections_after = [metric({"service": "products", "backend_id": "products-1", "attempt": "initial"}, selection_after)]
        for name, health, selections in (
            ("prometheus-before.json", health_before, selections_before),
            ("prometheus-after.json", health_after, selections_after),
        ):
            snapshot = {
                "queries": {
                    "backend_health": health,
                    "selections": selections,
                    "rate_limit_rejections": [metric({"reason": "exhausted"}, 0 if name == "prometheus-before.json" else 2)],
                    "gateway_process_start_time": [metric({"instance": "127.0.0.1:9091"}, 900)],
                }
            }
            (directory / name).write_text(json.dumps(snapshot), encoding="utf-8")

    def test_valid_runs_report_failure_rate_and_selection_delta(self):
        rr = load_run(self.rr_dir)
        adaptive = load_run(self.adaptive_dir)
        report = compare_runs(rr, adaptive)
        self.assertEqual(rr["failure_rate_percent"], 12)
        self.assertEqual(rr["backend_selection_delta"]["products/products-1/initial"], 20)
        self.assertEqual(rr["rate_limit_rejection_delta"]["exhausted"], 2)
        self.assertIn("effective_gateway_config", report["round_robin"])

    def test_rejects_mismatched_backend_urls_even_when_health_metrics_match(self):
        config = gateway_config()
        config["backend_pools"]["products"]["urls"][1] = "http://localhost:9001"
        target = self.root / "adaptive_diff_pool"
        self.write_run(target, "adaptive", 110, config=config)
        with self.assertRaisesRegex(ValueError, "effective_gateway_config"):
            compare_runs(load_run(self.rr_dir), load_run(target))

    def test_rejects_rate_limit_or_retry_configuration_changes(self):
        for section, key, value in (
            ("rate_limit", "rate_tokens_per_second", 500.0),
            ("retry", "max_retries", 1),
            ("circuit_breaker", "failure_threshold", 3),
        ):
            with self.subTest(section=section, key=key):
                config = gateway_config()
                config[section][key] = value
                target = self.root / f"adaptive_{section}_{key}"
                self.write_run(target, "adaptive", 110, config=config)
                with self.assertRaisesRegex(ValueError, "effective_gateway_config|Retry settings"):
                    compare_runs(load_run(self.rr_dir), load_run(target))

    def test_rejects_workload_warmup_and_service_address_mismatches(self):
        changes = (
            {"users": 21},
            {"spawn_rate": 6},
            {"duration": "6m"},
            {"warmup": 45},
            {"gateway_url": "http://127.0.0.1:8081"},
            {"prometheus_url": "http://127.0.0.1:9091"},
        )
        for options in changes:
            with self.subTest(options=options):
                target = self.root / f"adaptive_case_{len(list(self.root.iterdir()))}"
                self.write_run(target, "adaptive", 110, **options)
                with self.assertRaisesRegex(ValueError, "differing settings"):
                    compare_runs(load_run(self.rr_dir), load_run(target))

    def test_rejects_incomplete_or_malformed_csv_and_uncompleted_run(self):
        failed = self.root / "failed"
        self.write_run(failed, "adaptive", 110, status="failed")
        with self.assertRaisesRegex(ValueError, "does not mark"):
            load_run(failed)
        incomplete = self.root / "incomplete"
        self.write_run(incomplete, "adaptive", 110, malformed_csv=True)
        with self.assertRaisesRegex(ValueError, "missing required columns"):
            load_run(incomplete)

    def test_detects_selection_counter_reset_and_health_change(self):
        reset = self.root / "reset"
        self.write_run(reset, "adaptive", 110, selection_before=100, selection_after=10)
        with self.assertRaisesRegex(ValueError, "counter reset"):
            load_run(reset)

        changed_health = self.root / "health_changed"
        after = [metric({"service": "products", "backend_id": "products-1"}, 0)]
        self.write_run(changed_health, "adaptive", 110, health_after=after)
        with self.assertRaisesRegex(ValueError, "health changed"):
            load_run(changed_health)

    def test_rejects_actual_measurement_windows_outside_tolerance(self):
        target = self.root / "long_adaptive"
        self.write_run(target, "adaptive", 110, measured_elapsed=310)
        with self.assertRaisesRegex(ValueError, "actual measured windows"):
            compare_runs(load_run(self.rr_dir), load_run(target))

    def test_rejects_locust_and_workload_changes(self):
        target = self.root / "changed_workload"
        self.write_run(target, "adaptive", 110)
        metadata_path = target / "metadata.json"
        metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
        metadata["locustfile_sha256"] = "d" * 64
        metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "differing settings"):
            compare_runs(load_run(self.rr_dir), load_run(target))


if __name__ == "__main__":
    unittest.main()
