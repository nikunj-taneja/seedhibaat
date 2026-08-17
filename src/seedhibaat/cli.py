from __future__ import annotations

import argparse
import csv
import datetime as dt
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from .operator import OperatorError, daemon_request, register_operator_commands


class SeedhiBaatError(Exception):
    pass


@dataclass(frozen=True)
class Recipient:
    phone: str
    values: dict[str, str]


def normalize_phone(value: str, default_country_code: str = "") -> str:
    raw = value.strip()
    if not raw:
        raise SeedhiBaatError("phone number is empty")

    digits = re.sub(r"[\s()\-.]", "", raw)
    if digits.startswith("+"):
        digits = digits[1:]
    elif digits.startswith("00"):
        digits = digits[2:]
    elif default_country_code:
        country = default_country_code.strip().lstrip("+")
        if not country.isdigit() or country.startswith("0"):
            raise SeedhiBaatError("default country code must contain digits without a leading zero")
        if not digits.startswith(country):
            digits = country + digits

    if not re.fullmatch(r"[1-9]\d{7,14}", digits):
        raise SeedhiBaatError(f"not a valid E.164 phone number: {value!r}")
    return digits


def read_recipients(
    csv_path: Path, phone_column: str, default_country_code: str = ""
) -> list[str]:
    return [
        recipient.phone
        for recipient in read_recipient_rows(
            csv_path, phone_column, [], default_country_code
        )
    ]


def read_recipient_rows(
    csv_path: Path,
    phone_column: str,
    parameter_columns: Iterable[str],
    default_country_code: str = "",
) -> list[Recipient]:
    if not csv_path.is_file():
        raise SeedhiBaatError(f"CSV file not found: {csv_path}")

    columns = list(dict.fromkeys(parameter_columns))
    recipients: list[Recipient] = []
    seen: set[tuple[str, tuple[tuple[str, str], ...]]] = set()
    errors: list[str] = []
    with csv_path.open(newline="", encoding="utf-8-sig") as handle:
        reader = csv.DictReader(handle)
        if not reader.fieldnames or phone_column not in reader.fieldnames:
            available = ", ".join(reader.fieldnames or []) or "none"
            raise SeedhiBaatError(
                f"missing phone column {phone_column!r}; available columns: {available}"
            )
        duplicate_headers = sorted(
            {name for name in reader.fieldnames if reader.fieldnames.count(name) > 1}
        )
        if duplicate_headers:
            raise SeedhiBaatError(
                "duplicate CSV columns: " + ", ".join(repr(name) for name in duplicate_headers)
            )
        missing_columns = [column for column in columns if column not in reader.fieldnames]
        if missing_columns:
            raise SeedhiBaatError(
                "missing parameter columns: " + ", ".join(repr(name) for name in missing_columns)
            )
        for row_number, row in enumerate(reader, start=2):
            try:
                phone = normalize_phone(
                    row.get(phone_column, ""), default_country_code
                )
            except SeedhiBaatError as exc:
                errors.append(f"row {row_number}: {exc}")
                continue
            values = {column: (row.get(column) or "").strip() for column in columns}
            empty_columns = [column for column, value in values.items() if not value]
            if empty_columns:
                errors.append(
                    f"row {row_number}: empty template parameter columns: "
                    + ", ".join(repr(name) for name in empty_columns)
                )
                continue
            identity = (phone, tuple(values.items()))
            if identity not in seen:
                seen.add(identity)
                recipients.append(Recipient(phone=phone, values=values))

    if errors:
        preview = "\n".join(errors[:10])
        suffix = f"\n...and {len(errors) - 10} more" if len(errors) > 10 else ""
        raise SeedhiBaatError(f"invalid phone numbers:\n{preview}{suffix}")
    if not recipients:
        raise SeedhiBaatError("CSV contains no recipients")
    return recipients


def record_message(record: dict[str, object], *, category: str) -> dict[str, object] | None:
    """Put one CLI send into the daemon ledger, returning the daemon's response
    or None when it could not be recorded. Never raises: a recording failure
    must not stop a send that Meta has already accepted."""
    payload = {
        "phone": str(record["phone"]),
        "template": str(record["template"]),
        "language": str(record["language"]),
        "category": str(record.get("category") or category),
        "idempotency_key": str(record["idempotency_key"]),
        "parameter_fingerprint": str(record.get("parameter_fingerprint", "")),
        "status": "accepted" if record.get("status") == "accepted" else "failed",
        "attempted_at": str(record["timestamp"]),
    }
    if record.get("message_id"):
        payload["meta_message_id"] = str(record["message_id"])
    if record.get("error"):
        payload["failure_reason"] = str(record["error"])
        match = re.search(r"\"code\":\s*(\d+)", str(record["error"]))
        if match:
            payload["failure_code"] = match.group(1)
    try:
        response = daemon_request("/api/v1/messages/record", method="POST", payload=payload)
        return response if isinstance(response, dict) else {}
    except (OperatorError, urllib.error.URLError, json.JSONDecodeError, OSError):
        return None


def daemon_reachable() -> bool:
    base_url = os.environ.get("SEEDHIBAAT_OPERATOR_URL", "http://127.0.0.1:8088")
    try:
        with urllib.request.urlopen(base_url.rstrip("/") + "/healthz", timeout=10) as response:
            return response.status == 200
    except (urllib.error.URLError, OSError):
        return False


def run_ledger_sync(args: argparse.Namespace) -> int:
    ledger_path = args.ledger or Path.cwd() / "state" / "sends.ndjson"
    if not ledger_path.exists():
        raise SeedhiBaatError(f"ledger not found: {ledger_path}")
    if not daemon_reachable():
        raise SeedhiBaatError(
            "daemon is unreachable; start it or set SEEDHIBAAT_OPERATOR_URL to a tunnel"
        )
    recorded = already = unresolved = unknown = malformed = 0
    with ledger_path.open(encoding="utf-8") as handle:
        for line in handle:
            if not line.strip():
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                malformed += 1
                continue
            if args.since and str(record.get("timestamp", "")) < args.since:
                continue
            response = record_message(record, category=args.category)
            if response is None:
                unresolved += 1
            elif response.get("reason") == "unknown_recipient":
                unknown += 1
            elif response.get("recorded") or response.get("upgraded"):
                recorded += 1
            else:
                already += 1
    print(f"Newly recorded: {recorded}; already present: {already}; failed: {unresolved}")
    if unknown:
        print(
            f"{unknown} record(s) name a recipient with no customer row. They stay unrecorded "
            "until the recipient is synced from Shopify or their consent is imported."
        )
    if malformed:
        print(f"{malformed} ledger line(s) could not be parsed and were skipped.")
    if unresolved:
        print("Re-run after fixing the daemon connection; recording is idempotent.")
    return 1 if unresolved else 0


def build_template_parameters(
    values: dict[str, str],
    header_columns: Iterable[str],
    body_columns: Iterable[str],
) -> dict[str, str]:
    """Turn one CSV row into the daemon's parameter map.

    Slots are named `header.N` and `body.N` in template order. Each value is a
    `literal:`, because it comes from the row rather than from the customer or
    order record the daemon holds.
    """
    parameters: dict[str, str] = {}
    for index, column in enumerate(header_columns, start=1):
        parameters[f"header.{index}"] = "literal:" + values[column]
    for index, column in enumerate(body_columns, start=1):
        parameters[f"body.{index}"] = "literal:" + values[column]
    return parameters


def display_preview(
    recipients: Iterable[Recipient],
    template: str,
    language: str,
    header_image_url: str = "",
) -> None:
    recipients = list(recipients)
    unique_phones = list(dict.fromkeys(recipient.phone for recipient in recipients))
    print(f"Template: {template} ({language})")
    print(f"Validated messages: {len(recipients)}")
    print(f"Unique valid recipients: {len(unique_phones)}")
    if header_image_url:
        print(f"Header image: {header_image_url}")


def run_send(args: argparse.Namespace) -> int:
    """Create a campaign in the daemon and, on approval, activate it.

    The daemon is the only sender. It owns the outbound gate, consent and
    suppression checks, quiet hours, the frequency cap, and the message ledger,
    so nothing here talks to Meta.
    """
    header_columns = args.header_param or []
    body_columns = args.body_param or []
    header_image_url = (args.header_image_url or "").strip()
    tracked_url = (args.tracked_url or "").strip()
    if header_image_url:
        parsed_image_url = urllib.parse.urlparse(header_image_url)
        if (
            parsed_image_url.scheme != "https"
            or not parsed_image_url.netloc
            or parsed_image_url.username
            or parsed_image_url.password
        ):
            raise SeedhiBaatError(
                "header image must be an absolute HTTPS URL without credentials"
            )
        if header_columns:
            raise SeedhiBaatError(
                "an image header cannot be combined with text header parameters"
            )
    parameter_columns = [*header_columns, *body_columns]
    recipients = read_recipient_rows(
        args.csv,
        args.phone_column,
        parameter_columns,
        args.default_country_code
        or os.environ.get("SEEDHIBAAT_DEFAULT_COUNTRY_CODE", ""),
    )
    phones = [recipient.phone for recipient in recipients]
    duplicates = sorted({phone for phone in phones if phones.count(phone) > 1})
    if duplicates:
        # Two rows for one number carry two sets of values, and there is no way
        # to know which the operator meant. Numbers stay out of the message.
        raise SeedhiBaatError(
            f"{len(duplicates)} phone number(s) appear on more than one CSV row with "
            "different template values; keep one row per recipient"
        )

    display_preview(recipients, args.template, args.language, header_image_url)
    if tracked_url:
        print(f"Tracked destination: {tracked_url}")
    if parameter_columns:
        for index, column in enumerate(header_columns, start=1):
            print(f"Header parameter {index}: CSV column {column!r}")
        for index, column in enumerate(body_columns, start=1):
            print(f"Body parameter {index}: CSV column {column!r}")

    if not daemon_reachable():
        raise SeedhiBaatError(
            "daemon is unreachable, so the audience cannot be frozen and no message "
            "could be accounted for. Start the daemon or point SEEDHIBAAT_OPERATOR_URL "
            "at it. The CLI has no send path of its own."
        )

    segment = {
        "kind": "frozen_phones",
        "require_whatsapp_consent": True,
        "phones": phones,
    }
    if not args.send:
        result = daemon_request(
            "/api/v1/segments/preview", method="POST", payload=segment
        )
        eligible = int(result.get("eligible_count", 0)) if isinstance(result, dict) else 0
        print(f"Daemon would send to: {eligible}")
        _report_audience_gap(len(phones), eligible)
        print("Dry run only. Nothing was created. Add --send to draft the campaign.")
        return 0

    campaign_params = build_template_parameters(
        recipients[0].values, header_columns, body_columns
    )
    recipient_params = [
        {
            "phone": recipient.phone,
            "params": build_template_parameters(
                recipient.values, header_columns, body_columns
            ),
        }
        for recipient in recipients
    ] if parameter_columns else []

    draft = daemon_request(
        "/api/v1/campaigns",
        method="POST",
        payload={
            "name": args.name or f"{args.template} {dt.datetime.now(dt.timezone.utc):%Y-%m-%dT%H:%M:%SZ}",
            "segment": segment,
            "template": args.template,
            "language": args.language,
            "tracked_url": tracked_url,
            "header_image_url": header_image_url,
            "params": campaign_params,
            "recipient_params": recipient_params,
            "scheduled_at": args.scheduled_at or "",
            "frequency_messages": args.frequency_messages,
            "frequency_window": args.frequency_window,
        },
    )
    if not isinstance(draft, dict) or not draft.get("id"):
        raise SeedhiBaatError(f"the daemon did not return a draft campaign: {draft}")
    campaign_id = str(draft["id"])
    frozen = int(draft.get("audience_count", 0))
    print()
    print(f"Draft campaign: {campaign_id}")
    print(f"Frozen recipients: {frozen}")
    print(f"Recipients with their own values: {draft.get('recipients_with_parameters', 0)}")
    moot = int(draft.get("recipient_parameters_not_in_audience", 0))
    if moot:
        print(
            f"{moot} CSV row(s) carry values for a recipient who is not in the audience. "
            "Nothing is sent to them, so those values are unused."
        )
    _report_audience_gap(len(phones), frozen)
    if frozen == 0:
        print("Nothing to send. The draft stays unactivated.")
        return 1

    if not args.yes:
        if not sys.stdin.isatty():
            raise SeedhiBaatError("non-interactive activation requires --send --yes")
        confirmation = input(
            f"Activate {campaign_id}: template {args.template} ({args.language}), "
            f"MARKETING, to {frozen} recipients? Type SEND: "
        )
        if confirmation != "SEND":
            print(f"Cancelled. Draft {campaign_id} was left unactivated.")
            return 1

    activated = daemon_request(
        f"/api/v1/campaigns/{urllib.parse.quote(campaign_id, safe='')}/activate",
        method="POST",
        payload={
            "confirmed_recipient_count": frozen,
            "name": "",
            "segment": {"kind": "not_reordered", "require_whatsapp_consent": True},
            "template": "",
            "language": "",
        },
    )
    print(json.dumps(activated, indent=2))
    print(
        "Queued in the daemon. Messages are accounted for before Meta is called; "
        f"follow them with: seedhibaat campaign show --campaign-id {campaign_id}"
    )
    return 0


def _report_audience_gap(csv_rows: int, eligible: int) -> None:
    """Say plainly how many CSV rows the daemon will not send to.

    A number the daemon cannot match, cannot send to, or has no consent for is
    silently dropped from the audience. That gap must never be a surprise.
    """
    missing = csv_rows - eligible
    if missing > 0:
        print(
            f"{missing} of {csv_rows} CSV row(s) are not in the audience. They have no "
            "customer record, no WhatsApp consent, or are suppressed or invalid. "
            "Import them first with: seedhibaat customers import --csv <file> --confirm"
        )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="seedhibaat")
    subparsers = parser.add_subparsers(dest="command", required=True)
    send = subparsers.add_parser(
        "send", help="draft a campaign from a CSV recipient list and activate it"
    )
    send.add_argument("--csv", type=Path, required=True)
    send.add_argument("--phone-column", default="phone")
    send.add_argument(
        "--default-country-code",
        help="prefix local-format numbers with this country code; E.164 input is preferred",
    )
    send.add_argument("--template", required=True)
    send.add_argument("--language", default="en_US")
    send.add_argument("--name", help="campaign name; defaults to the template and the time")
    send.add_argument(
        "--header-image-url",
        help="public HTTPS JPEG or PNG supplied to an approved IMAGE header",
    )
    send.add_argument(
        "--tracked-url",
        help="HTTPS destination behind the template's dynamic URL button; the daemon "
        "issues one signed token per message and counts the clicks",
    )
    send.add_argument(
        "--header-param",
        action="append",
        metavar="CSV_COLUMN",
        help="map a CSV column to the next text header parameter; repeat in template order",
    )
    send.add_argument(
        "--body-param",
        action="append",
        metavar="CSV_COLUMN",
        help="map a CSV column to the next text body parameter; repeat in template order",
    )
    send.add_argument("--scheduled-at", help="RFC3339 time to queue the messages for")
    send.add_argument("--frequency-messages", type=int, default=1)
    send.add_argument("--frequency-window", default="24h")
    send.add_argument("--send", action="store_true", help="create the draft campaign")
    send.add_argument(
        "--yes", action="store_true", help="confirm activation without the typed prompt"
    )
    send.set_defaults(func=run_send)

    ledger = subparsers.add_parser("ledger", help="reconcile the local send ledger with the daemon")
    ledger_actions = ledger.add_subparsers(dest="ledger_command", required=True)
    ledger_sync = ledger_actions.add_parser(
        "sync", help="record ledger sends in the daemon so they appear in reporting and attribution"
    )
    ledger_sync.add_argument("--ledger", type=Path, help="defaults to state/sends.ndjson")
    ledger_sync.add_argument("--since", help="only replay records at or after this RFC3339 timestamp")
    ledger_sync.add_argument("--category", default="MARKETING")
    ledger_sync.set_defaults(func=run_ledger_sync)

    register_operator_commands(subparsers)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except (SeedhiBaatError, OperatorError, json.JSONDecodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
