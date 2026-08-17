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
    build_template_components,
    idempotency_key,
    main,
    normalize_phone,
    read_recipient_rows,
    build_parser,
    read_recipients,
    record_message,
    require_env,
    send_template,
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
    def test_builds_header_body_and_dynamic_url_components(self):
        components = build_template_components(
            {
                "customer": "Asha",
                "order_id": "A-1",
                "tracking_token": "signed-token",
            },
            ["customer"],
            ["order_id"],
            [(0, "tracking_token")],
        )
        self.assertEqual(
            components,
            [
                {"type": "header", "parameters": [{"type": "text", "text": "Asha"}]},
                {"type": "body", "parameters": [{"type": "text", "text": "A-1"}]},
                {
                    "type": "button",
                    "sub_type": "url",
                    "index": "0",
                    "parameters": [{"type": "text", "text": "signed-token"}],
                },
            ],
        )

    def test_builds_image_header_before_body_parameters(self):
        components = build_template_components(
            {"price": "750"},
            [],
            ["price"],
            [],
            "https://cdn.example.com/header.jpg",
        )
        self.assertEqual(
            components,
            [
                {
                    "type": "header",
                    "parameters": [
                        {
                            "type": "image",
                            "image": {
                                "link": "https://cdn.example.com/header.jpg"
                            },
                        }
                    ],
                },
                {
                    "type": "body",
                    "parameters": [{"type": "text", "text": "750"}],
                },
            ],
        )

    def test_send_payload_includes_rendered_components(self):
        components = [
            {"type": "body", "parameters": [{"type": "text", "text": "A-1"}]}
        ]
        response = io.BytesIO(b'{"messages":[{"id":"wamid.1"}]}')
        with patch("urllib.request.urlopen", return_value=response) as urlopen:
            message_id = send_template(
                phone="919876543210",
                template="order_update",
                language="en_US",
                components=components,
                env={
                    "META_SYSTEM_USER_TOKEN": "not-a-real-token",
                    "META_GRAPH_API_VERSION": "v23.0",
                    "WHATSAPP_PHONE_NUMBER_ID": "123",
                },
                timeout=30.0,
            )
        self.assertEqual(message_id, "wamid.1")
        request = urlopen.call_args.args[0]
        payload = json.loads(request.data)
        self.assertEqual(payload["template"]["components"], components)


class SafetyTests(unittest.TestCase):
    def test_dry_run_does_not_require_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text("phone\n9876543210\n", encoding="utf-8")
            with patch.dict(os.environ, {}, clear=True):
                self.assertEqual(
                    main(["send", "--csv", str(path), "--template", "order_update"]),
                    0,
                )

    def test_live_configuration_requires_every_identity_value(self):
        with patch.dict(os.environ, {"META_APP_ID": "123"}, clear=True):
            with self.assertRaisesRegex(SeedhiBaatError, "META_SYSTEM_USER_TOKEN"):
                require_env()

    def test_idempotency_key_changes_with_template(self):
        first = idempotency_key("919876543210", "order_update", "en_US")
        second = idempotency_key("919876543210", "shipping_update", "en_US")
        self.assertNotEqual(first, second)

    def test_idempotency_key_changes_with_rendered_parameters(self):
        first = idempotency_key(
            "919876543210",
            "order_update",
            "en_US",
            [{"type": "body", "parameters": [{"type": "text", "text": "A-1"}]}],
        )
        second = idempotency_key(
            "919876543210",
            "order_update",
            "en_US",
            [{"type": "body", "parameters": [{"type": "text", "text": "A-2"}]}],
        )
        self.assertNotEqual(first, second)

    def test_parameterized_dry_run_does_not_require_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,order_id,tracking_token\n9876543210,A-1,signed-token\n",
                encoding="utf-8",
            )
            with patch.dict(os.environ, {}, clear=True):
                self.assertEqual(
                    main(
                        [
                            "send",
                            "--csv",
                            str(path),
                            "--template",
                            "order_update",
                            "--body-param",
                            "order_id",
                            "--url-button-param",
                            "0:tracking_token",
                        ]
                    ),
                    0,
                )

    def test_rejects_duplicate_url_button_indexes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,first,second\n9876543210,a,b\n",
                encoding="utf-8",
            )

    def test_rejects_insecure_or_conflicting_image_header(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "recipients.csv"
            path.write_text(
                "phone,name\n9876543210,Asha\n",
                encoding="utf-8",
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
            self.assertEqual(
                main(
                    [
                        "send",
                        "--csv",
                        str(path),
                        "--template",
                        "order_update",
                        "--url-button-param",
                        "0:first",
                        "--url-button-param",
                        "0:second",
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

    def test_live_send_refuses_when_the_daemon_cannot_record_it(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            stderr = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=False):
                with patch("sys.stderr", stderr):
                    with patch("seedhibaat.cli.send_template") as send:
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
            self.assertIn("could not be recorded", stderr.getvalue())
            send.assert_not_called()

    def test_dry_run_does_not_need_the_daemon(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            with patch("seedhibaat.cli.daemon_reachable", return_value=False) as reachable:
                self.assertEqual(
                    main(["send", "--csv", str(path), "--template", "order_update"]), 0
                )
            reachable.assert_not_called()

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

    def test_there_is_no_flag_to_send_without_recording(self):
        parser = build_parser()
        with self.assertRaises(SystemExit):
            parser.parse_args(
                ["send", "--csv", "x.csv", "--template", "t", "--skip-daemon-ledger"]
            )

    def test_no_message_is_sent_when_the_daemon_will_not_account_for_it(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            stdout = io.StringIO()
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.require_env", return_value={}):
                    with patch(
                        "seedhibaat.cli.reserve_message",
                        return_value={"reserved": False, "reason": "unknown_recipient"},
                    ):
                        with patch("seedhibaat.cli.send_template") as send:
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
                                        "--send",
                                        "--yes",
                                    ]
                                )
            send.assert_not_called()
            self.assertIn("no customer record", stdout.getvalue())

    def test_nothing_is_sent_when_the_reservation_call_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.require_env", return_value={}):
                    with patch("seedhibaat.cli.reserve_message", return_value=None):
                        with patch("seedhibaat.cli.send_template") as send:
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
                                        "--send",
                                        "--yes",
                                    ]
                                )
            send.assert_not_called()

    def test_a_resend_is_accounted_for_separately(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self._csv(directory)
            reserved = []

            def reserve(base):
                reserved.append(str(base["idempotency_key"]))
                return {"reserved": True, "message_id": "msg_1"}

            with patch("seedhibaat.cli.daemon_reachable", return_value=True):
                with patch("seedhibaat.cli.require_env", return_value={}):
                    with patch("seedhibaat.cli.reserve_message", side_effect=reserve):
                        with patch("seedhibaat.cli.send_template", return_value="wamid.1"):
                            with patch("seedhibaat.cli.record_message", return_value={"recorded": True}):
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
                                            "--send",
                                            "--yes",
                                            "--allow-resend",
                                        ]
                                    )
            self.assertEqual(len(reserved), 1)
            self.assertIn(":resend:", reserved[0])
