import csv
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from seedhibaat.cli import (
    SeedhiBaatError,
    build_parser,
    build_template_parameters,
    main,
    normalize_phone,
    read_recipient_rows,
    read_recipients,
    record_message,
)


class PhoneTests(unittest.TestCase):
    def test_normalizes_e164_and_optional_local_formats(self):
        self.assertEqual(normalize_phone("98765 43210", "91"), "919876543210")
        self.assertEqual(normalize_phone("+91-98765-43210"), "919876543210")
        self.assertEqual(normalize_phone("0091 9876543210"), "919876543210")
        self.assertEqual(normalize_phone("+1 (415) 555-2671"), "14155552671")

    def test_rejects_numbers_that_are_not_e164(self):
        with self.assertRaises(SeedhiBaatError):
            normalize_phone("01234567")
        with self.assertRaises(SeedhiBaatError):
            normalize_phone("local-number")


class CsvTests(unittest.TestCase):
    def test_deduplicates_recipients(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            with path.open("w", newline="", encoding="utf-8") as handle:
                writer = csv.DictWriter(handle, fieldnames=["phone"])
                writer.writeheader()
                writer.writerow({"phone": "9876543210"})
                writer.writerow({"phone": "+919876543210"})
            self.assertEqual(read_recipients(path, "phone", "91"), ["919876543210"])

    def test_rejects_entire_file_if_any_row_is_invalid(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone\n9876543210\nbad\n", encoding="utf-8")
            with self.assertRaisesRegex(SeedhiBaatError, "row 3"):
                read_recipients(path, "phone", "91")

    def test_keeps_same_phone_when_parameter_values_differ(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,order_id\n9876543210,A-1\n9876543210,A-2\n9876543210,A-2\n",
                encoding="utf-8",
            )
            recipients = read_recipient_rows(path, "phone", ["order_id"], "91")
            self.assertEqual(len(recipients), 2)
            self.assertEqual([row.values["order_id"] for row in recipients], ["A-1", "A-2"])

    def test_rejects_missing_or_empty_parameter_columns(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone,order_id\n9876543210,\n", encoding="utf-8")
            with self.assertRaisesRegex(SeedhiBaatError, "empty template parameter"):
                read_recipient_rows(path, "phone", ["order_id"], "91")
            with self.assertRaisesRegex(SeedhiBaatError, "missing parameter columns"):
                read_recipient_rows(path, "phone", ["tracking_token"], "91")


class ParameterTests(unittest.TestCase):
    def test_maps_csv_columns_onto_ordered_daemon_parameter_slots(self):
        parameters = build_template_parameters(
            {"customer": "Asha", "order_id": "A-1", "delivered_on": "Monday"},
            ["customer"],
            ["order_id", "delivered_on"],
        )
        self.assertEqual(
            parameters,
            {
                "header.1": "literal:Asha",
                "body.1": "literal:A-1",
                "body.2": "literal:Monday",
            },
        )

    def test_a_template_without_parameters_maps_to_an_empty_slot_map(self):
        self.assertEqual(build_template_parameters({}, [], []), {})


class SafetyTests(unittest.TestCase):
    def test_dry_run_previews_the_segment_and_creates_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone\n9876543210\n", encoding="utf-8")
            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch(
                    "seedhibaat.cli.daemon_request", return_value={"eligible_count": 1}
                ) as request:
                    with patch("sys.stdout", stdout):
                        self.assertEqual(
                            main(
                                [
                                    "send",
                                    "--csv",
                                    str(path),
                                    "--template",
                                    "order_update",
                                    "--default-country-code",
                                    "91",
                                ]
                            ),
                            0,
                        )
            self.assertEqual(request.call_count, 1)
            self.assertEqual(request.call_args.args[0], "/api/v1/segments/preview")
            self.assertIn("Nothing was created", stdout.getvalue())

    def test_dry_run_names_how_many_rows_fall_outside_the_audience(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone\n9876543210\n9876543211\n9876543212\n", encoding="utf-8"
            )
            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch(
                    "seedhibaat.cli.daemon_request", return_value={"eligible_count": 1}
                ):
                    with patch("sys.stdout", stdout):
                        main(
                            [
                                "send",
                                "--csv",
                                str(path),
                                "--template",
                                "order_update",
                                "--default-country-code",
                                "91",
                            ]
                        )
            self.assertIn("2 of 3 CSV row(s) are not in the audience", stdout.getvalue())

    def test_the_segment_carries_plaintext_numbers_and_never_hashes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone\n9876543210\n", encoding="utf-8")
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch(
                    "seedhibaat.cli.daemon_request", return_value={"eligible_count": 1}
                ) as request:
                    with patch("sys.stdout", io.StringIO()):
                        main(
                            [
                                "send",
                                "--csv",
                                str(path),
                                "--template",
                                "order_update",
                                "--default-country-code",
                                "91",
                            ]
                        )
            payload = request.call_args.kwargs["payload"]
            self.assertEqual(payload["kind"], "frozen_phones")
            self.assertTrue(payload["require_whatsapp_consent"])
            self.assertEqual(payload["phones"], ["919876543210"])

    def test_rejects_one_number_that_carries_two_sets_of_values(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,order_id\n9876543210,A-1\n9876543210,A-2\n", encoding="utf-8"
            )
            stderr = io.StringIO()
            with patch("sys.stderr", stderr):
                code = main(
                    [
                        "send",
                        "--csv",
                        str(path),
                        "--template",
                        "order_update",
                        "--body-param",
                        "order_id",
                        "--default-country-code",
                        "91",
                    ]
                )
            self.assertEqual(code, 2)
            self.assertIn("more than one CSV row", stderr.getvalue())

    def test_rejects_insecure_or_conflicting_image_header(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone,name\n9876543210,Asha\n", encoding="utf-8")
            self.assertEqual(
                main(
                    [
                        "send",
                        "--csv",
                        str(path),
                        "--template",
                        "image_template",
                        "--header-image-url",
                        "http://cdn.example.com/header.jpg",
                    ]
                ),
                2,
            )
            self.assertEqual(
                main(
                    [
                        "send",
                        "--csv",
                        str(path),
                        "--template",
                        "image_template",
                        "--header-image-url",
                        "https://cdn.example.com/header.jpg",
                        "--header-param",
                        "name",
                    ]
                ),
                2,
            )


if __name__ == "__main__":
    unittest.main()


class LedgerTraceabilityTests(unittest.TestCase):
    def _csv(self, directory: str) -> Path:
        path = Path(directory) / "recipients.csv"
        path.write_text("phone\n9876543210\n", encoding="utf-8")
        return path

    def test_a_send_refuses_when_the_daemon_is_unreachable(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            stderr = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=False):
                with patch("seedhibaat.cli.daemon_request") as request:
                    with patch("sys.stderr", stderr):
                        code = main(
                            [
                                "send",
                                "--csv",
                                str(path),
                                "--template",
                                "order_update",
                                "--default-country-code",
                                "91",
                                "--send",
                                "--yes",
                            ]
                        )
            self.assertEqual(code, 2)
            self.assertIn("no send path of its own", stderr.getvalue())
            request.assert_not_called()

    def test_a_dry_run_also_needs_the_daemon_to_freeze_the_audience(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            stderr = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=False):
                with patch("sys.stderr", stderr):
                    self.assertEqual(
                        main(["send", "--csv", str(path), "--template", "order_update"]), 2
                    )
            self.assertIn("audience cannot be frozen", stderr.getvalue())

    def test_record_message_maps_a_failure_code_from_the_meta_error(self):
        record = {
            "timestamp": "2026-08-16T09:27:24Z",
            "phone": "+919876543210",
            "template": "stay_upgrade",
            "language": "en_US",
            "idempotency_key": "key",
            "parameter_fingerprint": "fp",
            "status": "failed",
            "error": 'Meta HTTP 400: {"error":{"message":"x","code":131049}}',
        }
        with patch("seedhibaat.cli.daemon_request", return_value={"recorded": True}) as request:
            self.assertEqual(record_message(record, category="MARKETING"), {"recorded": True})
        payload = request.call_args.kwargs["payload"]
        self.assertEqual(payload["failure_code"], "131049")
        self.assertEqual(payload["status"], "failed")
        self.assertNotIn("meta_message_id", payload)

    def test_record_message_reports_failure_instead_of_raising(self):
        record = {
            "timestamp": "2026-08-16T09:27:24Z",
            "phone": "+919876543210",
            "template": "stay_upgrade",
            "language": "en_US",
            "idempotency_key": "key",
            "status": "accepted",
            "message_id": "wamid.1",
        }
        with patch("seedhibaat.cli.daemon_request", side_effect=OSError("connection refused")):
            self.assertIsNone(record_message(record, category="MARKETING"))

    def test_ledger_sync_replays_every_line_and_counts_duplicates(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "sends.ndjson"
            ledger.write_text(
                json.dumps(
                    {
                        "timestamp": "2026-08-16T09:27:24Z",
                        "phone": "+919876543210",
                        "template": "stay_upgrade",
                        "language": "en_US",
                        "idempotency_key": "key-one",
                        "status": "accepted",
                        "message_id": "wamid.1",
                    }
                )
                + "\n"
                + json.dumps(
                    {
                        "timestamp": "2026-08-16T09:27:25Z",
                        "phone": "+919876543211",
                        "template": "stay_upgrade",
                        "language": "en_US",
                        "idempotency_key": "key-two",
                        "status": "accepted",
                        "message_id": "wamid.2",
                    }
                )
                + "\n",
                encoding="utf-8",
            )
            responses = [{"recorded": True}, {"recorded": False, "already_recorded": True}]
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.daemon_request", side_effect=responses):
                    self.assertEqual(
                        main(["ledger", "sync", "--ledger", str(ledger)]), 0
                    )

    def test_ledger_sync_refuses_when_the_daemon_is_down(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "sends.ndjson"
            ledger.write_text("", encoding="utf-8")
            stderr = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=False):
                with patch("sys.stderr", stderr):
                    code = main(["ledger", "sync", "--ledger", str(ledger)])
            self.assertEqual(code, 2)
            self.assertIn("unreachable", stderr.getvalue())

    def test_unknown_recipient_is_reported_not_counted_as_recorded(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "sends.ndjson"
            ledger.write_text(
                json.dumps(
                    {
                        "timestamp": "2026-08-16T09:27:24Z",
                        "phone": "+919876543210",
                        "template": "stay_upgrade",
                        "language": "en_US",
                        "idempotency_key": "key-one",
                        "status": "accepted",
                        "message_id": "wamid.1",
                    }
                )
                + "\n",
                encoding="utf-8",
            )
            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch(
                    "seedhibaat.cli.daemon_request",
                    return_value={"recorded": False, "reason": "unknown_recipient"},
                ):
                    with patch("sys.stdout", stdout):
                        self.assertEqual(main(["ledger", "sync", "--ledger", str(ledger)]), 0)
            self.assertIn("no customer row", stdout.getvalue())
            self.assertIn("Newly recorded: 0", stdout.getvalue())

    def test_ledger_sync_skips_a_truncated_line_instead_of_aborting(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "sends.ndjson"
            ledger.write_text(
                json.dumps(
                    {
                        "timestamp": "2026-08-16T09:27:24Z",
                        "phone": "+919876543210",
                        "template": "stay_upgrade",
                        "language": "en_US",
                        "idempotency_key": "key-one",
                        "status": "accepted",
                        "message_id": "wamid.1",
                    }
                )
                + "\n" + '{"timestamp":"2026-08-16T09:2',
                encoding="utf-8",
            )
            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.daemon_request", return_value={"recorded": True}):
                    with patch("sys.stdout", stdout):
                        self.assertEqual(main(["ledger", "sync", "--ledger", str(ledger)]), 0)
            self.assertIn("Newly recorded: 1", stdout.getvalue())
            self.assertIn("could not be parsed", stdout.getvalue())

    def test_replayed_record_keeps_its_original_category(self):
        record = {
            "timestamp": "2026-08-16T09:27:24Z",
            "phone": "+919876543210",
            "template": "stay_utility",
            "language": "en_US",
            "category": "UTILITY",
            "idempotency_key": "key",
            "status": "accepted",
            "message_id": "wamid.1",
        }
        with patch("seedhibaat.cli.daemon_request", return_value={"recorded": True}) as request:
            record_message(record, category="MARKETING")
        self.assertEqual(request.call_args.kwargs["payload"]["category"], "UTILITY")

    def test_the_cli_offers_no_way_to_send_outside_the_daemon(self):
        parser = build_parser()
        for flag in ("--skip-daemon-ledger", "--url-button-param", "--allow-resend"):
            with self.subTest(flag=flag):
                with self.assertRaises(SystemExit):
                    parser.parse_args(
                        ["send", "--csv", "x.csv", "--template", "t", flag, "0:c"]
                    )

    def test_a_campaign_is_drafted_and_then_activated_at_the_frozen_count(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,coupon\n9876543210,SAVE-A\n9876543211,SAVE-B\n", encoding="utf-8"
            )
            calls = []

            def request(endpoint, method="GET", payload=None):
                calls.append((endpoint, payload))
                if endpoint == "/api/v1/campaigns":
                    return {
                        "id": "campaign_1",
                        "audience_count": 2,
                        "recipients_with_parameters": 2,
                    }
                return {"id": "campaign_1", "state": "scheduled", "recipient_count": 2}

            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.daemon_request", side_effect=request):
                    with patch("sys.stdout", io.StringIO()):
                        self.assertEqual(
                            main(
                                [
                                    "send",
                                    "--csv",
                                    str(path),
                                    "--template",
                                    "order_update",
                                    "--body-param",
                                    "coupon",
                                    "--default-country-code",
                                    "91",
                                    "--send",
                                    "--yes",
                                ]
                            ),
                            0,
                        )
            self.assertEqual(
                [endpoint for endpoint, _ in calls],
                ["/api/v1/campaigns", "/api/v1/campaigns/campaign_1/activate"],
            )
            draft = calls[0][1]
            self.assertEqual(
                draft["recipient_params"],
                [
                    {"phone": "919876543210", "params": {"body.1": "literal:SAVE-A"}},
                    {"phone": "919876543211", "params": {"body.1": "literal:SAVE-B"}},
                ],
            )
            self.assertEqual(calls[1][1]["confirmed_recipient_count"], 2)

    def test_a_draft_with_no_eligible_recipient_is_never_activated(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            calls = []

            def request(endpoint, method="GET", payload=None):
                calls.append(endpoint)
                return {"id": "campaign_1", "audience_count": 0}

            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.daemon_request", side_effect=request):
                    with patch("sys.stdout", stdout):
                        self.assertEqual(
                            main(
                                [
                                    "send",
                                    "--csv",
                                    str(path),
                                    "--template",
                                    "order_update",
                                    "--default-country-code",
                                    "91",
                                    "--send",
                                    "--yes",
                                ]
                            ),
                            1,
                        )
            self.assertEqual(calls, ["/api/v1/campaigns"])
            self.assertIn("Nothing to send", stdout.getvalue())

    def test_activation_without_yes_needs_a_typed_confirmation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            calls = []

            def request(endpoint, method="GET", payload=None):
                calls.append(endpoint)
                return {"id": "campaign_1", "audience_count": 1}

            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.daemon_request", side_effect=request):
                    with patch("sys.stdin.isatty", return_value=True):
                        with patch("builtins.input", return_value="yes"):
                            with patch("sys.stdout", stdout):
                                self.assertEqual(
                                    main(
                                        [
                                            "send",
                                            "--csv",
                                            str(path),
                                            "--template",
                                            "order_update",
                                            "--default-country-code",
                                            "91",
                                            "--send",
                                        ]
                                    ),
                                    1,
                                )
            self.assertEqual(calls, ["/api/v1/campaigns"])
            self.assertIn("left unactivated", stdout.getvalue())
