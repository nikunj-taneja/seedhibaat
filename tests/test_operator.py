import argparse
import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

from seedhibaat.operator import (
    GENERATED_SECRET_NAMES,
    OperatorError,
    _segment_payload,
    _safe_error_detail,
    _load_template,
    run_campaign_activate,
    run_workflow_simulate,
    run_workflow_activate,
    run_secrets_init,
    run_template_media_upload,
    run_template_submit,
)


class TemplateOperatorTests(unittest.TestCase):
    def test_template_dry_run_never_calls_meta(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "template.json"
            path.write_text(json.dumps({
                "name": "approved_example",
                "language": "en_US",
                "category": "MARKETING",
                "components": [{"type": "BODY", "text": "Example message."}],
            }))
            args = argparse.Namespace(file=path, submit=False, yes=False)
            with patch("seedhibaat.operator._meta_request") as request:
                output = io.StringIO()
                with redirect_stdout(output):
                    self.assertEqual(run_template_submit(args), 0)
        request.assert_not_called()
        self.assertIn("Dry run only", output.getvalue())

    def test_rejects_unclassified_template(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "template.json"
            path.write_text(
                json.dumps(
                    {
                        "name": "bad",
                        "language": "en_US",
                        "category": "AUTHENTICATION",
                        "components": [],
                    }
                )
            )
            with self.assertRaisesRegex(OperatorError, "MARKETING or UTILITY"):
                _load_template(path)

    def test_media_header_preview_and_placeholder_submission_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "template.json"
            path.write_text(
                json.dumps(
                    {
                        "name": "image_example",
                        "language": "en_US",
                        "category": "MARKETING",
                        "components": [
                            {
                                "type": "HEADER",
                                "format": "IMAGE",
                                "example": {
                                    "header_handle": [
                                        "REPLACE_WITH_META_UPLOAD_HANDLE"
                                    ]
                                },
                            },
                            {"type": "BODY", "text": "Example message."},
                        ],
                    }
                )
            )
            output = io.StringIO()
            with redirect_stdout(output):
                self.assertEqual(
                    run_template_submit(
                        argparse.Namespace(
                            file=path,
                            header_handle_file=None,
                            submit=False,
                            yes=False,
                        )
                    ),
                    0,
                )
            self.assertIn("Header: IMAGE", output.getvalue())
            with patch("seedhibaat.operator._meta_request") as request:
                with self.assertRaisesRegex(OperatorError, "upload handle"):
                    run_template_submit(
                        argparse.Namespace(
                            file=path,
                            header_handle_file=None,
                            submit=True,
                            yes=True,
                        )
                    )
            request.assert_not_called()

            handle = Path(directory) / "handle"
            handle.write_text("media-handle\n", encoding="utf-8")
            with (
                patch.dict(
                    os.environ,
                    {"WHATSAPP_BUSINESS_ACCOUNT_ID": "waba"},
                ),
                patch(
                    "seedhibaat.operator._meta_request",
                    return_value={"id": "template-id"},
                ) as request,
            ):
                with redirect_stdout(io.StringIO()):
                    self.assertEqual(
                        run_template_submit(
                            argparse.Namespace(
                                file=path,
                                header_handle_file=handle,
                                submit=True,
                                yes=True,
                            )
                        ),
                        0,
                    )
            submitted = request.call_args.kwargs["payload"]
            self.assertEqual(
                submitted["components"][0]["example"]["header_handle"],
                ["media-handle"],
            )

    def test_media_upload_is_dry_by_default_and_writes_handle_privately(self):
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory) / "header.jpg"
            image.write_bytes(b"\xff\xd8\xff" + b"image")
            handle = Path(directory) / "handle"
            output = io.StringIO()
            with patch("seedhibaat.operator._meta_request") as request:
                with redirect_stdout(output):
                    self.assertEqual(
                        run_template_media_upload(
                            argparse.Namespace(
                                file=image,
                                handle_file=handle,
                                upload=False,
                                yes=False,
                            )
                        ),
                        0,
                    )
            request.assert_not_called()
            self.assertFalse(handle.exists())
            with (
                patch.dict(os.environ, {"META_APP_ID": "app"}),
                patch(
                    "seedhibaat.operator._meta_request",
                    return_value={"id": "upload:session"},
                ) as request,
                patch(
                    "seedhibaat.operator._meta_upload_bytes",
                    return_value={"h": "media-handle"},
                ) as upload,
                redirect_stdout(io.StringIO()),
            ):
                self.assertEqual(
                    run_template_media_upload(
                        argparse.Namespace(
                            file=image,
                            handle_file=handle,
                            upload=True,
                            yes=True,
                        )
                    ),
                    0,
                )
            request.assert_called_once()
            upload.assert_called_once_with("upload:session", image.read_bytes())
            self.assertEqual(handle.read_text().strip(), "media-handle")
            self.assertEqual(os.stat(handle).st_mode & 0o777, 0o600)


class CampaignSafetyTests(unittest.TestCase):
    def test_frozen_csv_reads_owner_only_shopify_ids(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "audience.csv"
            path.write_text(
                "customer_id\ngid://shopify/Customer/1\ngid://shopify/Customer/2\n",
                encoding="utf-8",
            )
            args = argparse.Namespace(
                kind="frozen_csv", customer_ids_file=path,
                product_handle=None, product_title=None, within_days=None,
                lapsed_days=None, allow_unknown_consent=False,
                exclude_product_handle=None, exclude_product_title=None,
                exclude_product_tag=None, exclude_recent_days=None,
            )
            payload = _segment_payload(args)
        self.assertEqual(payload["customer_shopify_ids"], [
            "gid://shopify/Customer/1", "gid://shopify/Customer/2"
        ])

    def test_activation_requires_explicit_flags(self):
        args = argparse.Namespace(
            campaign_id="campaign_test",
            confirmed_count=1,
            activate=False,
            yes=True,
        )
        with patch("seedhibaat.operator.daemon_request") as request:
            with self.assertRaisesRegex(OperatorError, "--activate --yes"):
                run_campaign_activate(args)
        request.assert_not_called()

    def test_workflow_simulation_passes_definition_without_mutating_flags(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yaml"
            path.write_text("name: example\n", encoding="utf-8")
            args = argparse.Namespace(
                file=path,
                triggered_at="2026-07-01T12:00:00Z",
                as_of="2026-07-02T12:00:00Z",
            )
            with patch(
                "seedhibaat.operator.daemon_request",
                return_value={"writes_performed": False},
            ) as request:
                output = io.StringIO()
                with redirect_stdout(output):
                    self.assertEqual(run_workflow_simulate(args), 0)
        request.assert_called_once_with(
            "/api/v1/workflows/simulate",
            method="POST",
            payload={
                "yaml": "name: example\n",
                "triggered_at": "2026-07-01T12:00:00Z",
                "as_of": "2026-07-02T12:00:00Z",
            },
        )
        self.assertIn('"writes_performed": false', output.getvalue())

    def test_workflow_activation_requires_explicit_flags(self):
        args = argparse.Namespace(
            name="flow", confirmed_count=0, activate=False, yes=True
        )
        with patch("seedhibaat.operator.daemon_request") as request:
            with self.assertRaisesRegex(OperatorError, "--activate --yes"):
                run_workflow_activate(args)
        request.assert_not_called()


class SecretInitializationTests(unittest.TestCase):
    def test_generates_missing_values_without_overwriting_provider_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / ".env"
            path.write_text(
                "SHOPIFY_CLIENT_SECRET=provider-secret\nSEEDHIBAAT_API_KEY=\n"
            )
            output = io.StringIO()
            with redirect_stdout(output):
                self.assertEqual(
                    run_secrets_init(argparse.Namespace(env_file=path)), 0
                )
            body = path.read_text()
            self.assertIn("SHOPIFY_CLIENT_SECRET=provider-secret", body)
            self.assertNotIn("SEEDHIBAAT_API_KEY=\n", body)
            for name in GENERATED_SECRET_NAMES:
                self.assertRegex(body, rf"(?m)^{name}=.{{32,}}$")
            self.assertNotIn("provider-secret", output.getvalue())
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)

    def test_provider_error_details_are_redacted(self):
        prefix = "shp" + "ss_"
        detail = _safe_error_detail(
            f"recipient +91 99999 99999 token {prefix}abcdefghijklmnopqrstuvwxyz failed"
        )
        self.assertNotIn("99999", detail)
        self.assertNotIn(prefix, detail)


if __name__ == "__main__":
    unittest.main()
