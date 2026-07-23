from __future__ import annotations

import argparse
import csv
import datetime as dt
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from .operator import OperatorError, register_operator_commands


REQUIRED_ENV = (
    "META_APP_ID",
    "META_SYSTEM_USER_TOKEN",
    "WHATSAPP_PHONE_NUMBER_ID",
    "META_GRAPH_API_VERSION",
)


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


def require_env() -> dict[str, str]:
    values = {name: os.environ.get(name, "").strip() for name in REQUIRED_ENV}
    missing = [name for name, value in values.items() if not value]
    if missing:
        raise SeedhiBaatError("missing required environment variables: " + ", ".join(missing))
    if not re.fullmatch(r"v\d+\.\d+", values["META_GRAPH_API_VERSION"]):
        raise SeedhiBaatError("META_GRAPH_API_VERSION must look like v23.0")
    return values


def load_sent_keys(path: Path) -> set[str]:
    if not path.exists():
        return set()
    keys: set[str] = set()
    with path.open(encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                record = json.loads(line)
                if record.get("status") == "accepted":
                    keys.add(record["idempotency_key"])
    return keys


def idempotency_key(
    phone: str,
    template: str,
    language: str,
    components: list[dict[str, object]] | None = None,
) -> str:
    rendered = json.dumps(components or [], ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    material = f"{phone}\0{template}\0{language}\0{rendered}".encode()
    return hashlib.sha256(material).hexdigest()


def parameter_fingerprint(components: list[dict[str, object]]) -> str:
    rendered = json.dumps(components, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(rendered.encode()).hexdigest()


def parse_url_button_binding(value: str) -> tuple[int, str]:
    index_text, separator, column = value.partition(":")
    if not separator or not index_text.isdigit() or not column.strip():
        raise argparse.ArgumentTypeError("must look like BUTTON_INDEX:CSV_COLUMN, for example 0:tracking_token")
    return int(index_text), column.strip()


def build_template_components(
    values: dict[str, str],
    header_columns: Iterable[str],
    body_columns: Iterable[str],
    url_button_bindings: Iterable[tuple[int, str]],
    header_image_url: str = "",
) -> list[dict[str, object]]:
    components: list[dict[str, object]] = []
    if header_image_url:
        components.append(
            {
                "type": "header",
                "parameters": [
                    {"type": "image", "image": {"link": header_image_url}}
                ],
            }
        )
    header_parameters = [
        {"type": "text", "text": values[column]} for column in header_columns
    ]
    if header_parameters:
        components.append({"type": "header", "parameters": header_parameters})

    body_parameters = [
        {"type": "text", "text": values[column]} for column in body_columns
    ]
    if body_parameters:
        components.append({"type": "body", "parameters": body_parameters})

    for index, column in url_button_bindings:
        components.append(
            {
                "type": "button",
                "sub_type": "url",
                "index": str(index),
                "parameters": [{"type": "text", "text": values[column]}],
            }
        )
    return components


def append_jsonl(path: Path, record: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n")


def send_template(
    *,
    phone: str,
    template: str,
    language: str,
    components: list[dict[str, object]],
    env: dict[str, str],
    timeout: float,
) -> str:
    url = (
        "https://graph.facebook.com/"
        f"{env['META_GRAPH_API_VERSION']}/{env['WHATSAPP_PHONE_NUMBER_ID']}/messages"
    )
    template_payload: dict[str, object] = {
        "name": template,
        "language": {"code": language},
    }
    if components:
        template_payload["components"] = components
    body = json.dumps(
        {
            "messaging_product": "whatsapp",
            "recipient_type": "individual",
            "to": phone,
            "type": "template",
            "template": template_payload,
        }
    ).encode()
    request = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {env['META_SYSTEM_USER_TOKEN']}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise SeedhiBaatError(f"Meta HTTP {exc.code}: {detail}") from exc
    except urllib.error.URLError as exc:
        raise SeedhiBaatError(f"network error: {exc.reason}") from exc

    try:
        return payload["messages"][0]["id"]
    except (KeyError, IndexError, TypeError) as exc:
        raise SeedhiBaatError(f"unexpected Meta response: {payload}") from exc


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
    header_columns = args.header_param or []
    body_columns = args.body_param or []
    url_button_bindings = args.url_button_param or []
    header_image_url = (args.header_image_url or "").strip()
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
    button_indexes = [index for index, _ in url_button_bindings]
    duplicate_button_indexes = sorted(
        {index for index in button_indexes if button_indexes.count(index) > 1}
    )
    if duplicate_button_indexes:
        raise SeedhiBaatError(
            "duplicate URL button indexes: "
            + ", ".join(str(index) for index in duplicate_button_indexes)
        )
    parameter_columns = [
        *header_columns,
        *body_columns,
        *(column for _, column in url_button_bindings),
    ]
    recipients = read_recipient_rows(
        args.csv,
        args.phone_column,
        parameter_columns,
        args.default_country_code
        or os.environ.get("SEEDHIBAAT_DEFAULT_COUNTRY_CODE", ""),
    )
    display_preview(
        recipients, args.template, args.language, header_image_url
    )
    if not args.send:
        print("Dry run only. Add --send after reviewing the recipient count.")
        return 0

    if not args.yes:
        if not sys.stdin.isatty():
            raise SeedhiBaatError("non-interactive live sends require --send --yes")
        unique_recipient_count = len({recipient.phone for recipient in recipients})
        confirmation = input(
            f"Send {len(recipients)} messages to {unique_recipient_count} recipients? Type SEND: "
        )
        if confirmation != "SEND":
            print("Cancelled.")
            return 1

    env = require_env()
    root = Path.cwd()
    ledger_path = root / "state" / "sends.ndjson"
    sent_keys = load_sent_keys(ledger_path)
    timestamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_path = root / "runs" / f"{timestamp}.ndjson"
    accepted = failed = skipped = 0

    for recipient in recipients:
        components = build_template_components(
            recipient.values,
            header_columns,
            body_columns,
            url_button_bindings,
            header_image_url,
        )
        key = idempotency_key(
            recipient.phone, args.template, args.language, components
        )
        base: dict[str, object] = {
            "timestamp": dt.datetime.now(dt.timezone.utc).isoformat(),
            "phone": f"+{recipient.phone}",
            "template": args.template,
            "language": args.language,
            "idempotency_key": key,
        }
        if components:
            base["parameter_fingerprint"] = parameter_fingerprint(components)
        if key in sent_keys and not args.allow_resend:
            record = {**base, "status": "skipped_duplicate"}
            append_jsonl(run_path, record)
            skipped += 1
            continue
        try:
            message_id = send_template(
                phone=recipient.phone,
                template=args.template,
                language=args.language,
                components=components,
                env=env,
                timeout=args.timeout,
            )
            record = {**base, "status": "accepted", "message_id": message_id}
            append_jsonl(ledger_path, record)
            sent_keys.add(key)
            accepted += 1
        except SeedhiBaatError as exc:
            record = {**base, "status": "failed", "error": str(exc)}
            failed += 1
        append_jsonl(run_path, record)

    print(f"Accepted: {accepted}; failed: {failed}; skipped duplicates: {skipped}")
    print(f"Run log: {run_path}")
    return 1 if failed else 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="seedhibaat")
    subparsers = parser.add_subparsers(dest="command", required=True)
    send = subparsers.add_parser("send", help="validate or send a CSV recipient list")
    send.add_argument("--csv", type=Path, required=True)
    send.add_argument("--phone-column", default="phone")
    send.add_argument(
        "--default-country-code",
        help="prefix local-format numbers with this country code; E.164 input is preferred",
    )
    send.add_argument("--template", required=True)
    send.add_argument("--language", default="en_US")
    send.add_argument(
        "--header-image-url",
        help="public HTTPS JPEG or PNG supplied to an approved IMAGE header",
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
    send.add_argument(
        "--url-button-param",
        action="append",
        type=parse_url_button_binding,
        metavar="INDEX:CSV_COLUMN",
        help="map a CSV column to a dynamic URL button suffix, for example 0:tracking_token",
    )
    send.add_argument("--send", action="store_true", help="perform a live send")
    send.add_argument("--yes", action="store_true", help="confirm a non-interactive live send")
    send.add_argument("--allow-resend", action="store_true")
    send.add_argument("--timeout", type=float, default=30.0)
    send.set_defaults(func=run_send)
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
