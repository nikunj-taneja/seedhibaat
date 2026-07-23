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
    _safe_error_detail,
    _load_template,
    run_campaign_activate,
    run_workflow_simulate,
    run_workflow_activate,
    run_secrets_init,
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


class CampaignSafetyTests(unittest.TestCase):
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
