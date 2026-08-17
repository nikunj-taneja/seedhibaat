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

from .operator import OperatorError, daemon_request, register_operator_commands


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


def reserve_message(base: dict[str, object]) -> dict[str, object] | None:
    """Ask the daemon to account for a message before it is sent.

    Returns the daemon's answer, or None when the daemon could not be reached.
    A caller that does not get `reserved: true` must not send.
    """
    payload = {
        "phone": str(base["phone"]),
        "template": str(base["template"]),
        "language": str(base["language"]),
        "category": str(base.get("category") or "MARKETING"),
        "idempotency_key": str(base["idempotency_key"]),
        "parameter_fingerprint": str(base.get("parameter_fingerprint", "")),
        "attempted_at": str(base["timestamp"]),
    }
    try:
        response = daemon_request("/api/v1/messages/reserve", method="POST", payload=payload)
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

    # Every message must be traceable, so refuse to send when the daemon that
    # owns the ledger cannot be reached. Overriding this is deliberate and
    # leaves the run needing `ledger sync`.
    record_to_daemon = True
    if not daemon_reachable():
        raise SeedhiBaatError(
            "daemon is unreachable, so these sends could not be recorded or attributed. "
            "Start the daemon or point SEEDHIBAAT_OPERATOR_URL at it. There is no "
            "untracked send path: money spent must leave a record."
        )
    unrecorded = 0
    unknown_recipients = 0

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
            "category": args.category,
            "idempotency_key": key,
        }
        if components:
            base["parameter_fingerprint"] = parameter_fingerprint(components)
        if args.allow_resend:
            # A deliberate resend is its own message and must be accounted for
            # separately, not merged into the original send's record.
            key = key + ":resend:" + str(base["timestamp"])
            base["idempotency_key"] = key
        elif key in sent_keys:
            record = {**base, "status": "skipped_duplicate"}
            append_jsonl(run_path, record)
            skipped += 1
            continue

        # Account for the message before spending money on it. The daemon
        # refuses a recipient it cannot record, and a refusal means no send.
        reservation = reserve_message(base)
        if reservation is None:
            record = {**base, "status": "not_sent", "error": "daemon did not reserve the message"}
            append_jsonl(run_path, record)
            unrecorded += 1
            continue
        if not reservation.get("reserved"):
            reason = str(reservation.get("reason", "refused"))
            record = {**base, "status": "not_sent", "error": reason}
            append_jsonl(run_path, record)
            if reason == "unknown_recipient":
                unknown_recipients += 1
            else:
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
        except (SeedhiBaatError, OSError) as exc:
            # OSError covers socket timeouts, which send_template does not wrap;
            # letting one escape would abort the run after Meta may already have
            # accepted the message.
            record = {**base, "status": "failed", "error": str(exc)}
            append_jsonl(ledger_path, record)
            failed += 1
        # Settle the reservation. If this fails the reserved row stays in the
        # daemon as an unreconciled send, and `ledger sync` closes it.
        if record_message(record, category=args.category) is None:
            unrecorded += 1
        append_jsonl(run_path, record)

    print(f"Accepted: {accepted}; failed: {failed}; skipped duplicates: {skipped}")
    if record_to_daemon:
        print(f"Recorded in the daemon ledger: {accepted + failed - unrecorded} of {accepted + failed}")
        if unrecorded:
            print(
                f"WARNING: {unrecorded} message(s) are not in the daemon ledger. "
                "They will not appear in reporting or attribute revenue until you run: "
                "seedhibaat ledger sync"
            )
        if unknown_recipients:
            print(
                f"{unknown_recipients} recipient(s) have no customer record. Nothing was sent to "
                "them, because the send could not be accounted for. Import them first with: "
                "seedhibaat customers import --csv <file> --confirm"
            )
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
        "--category",
        default="MARKETING",
        help="template category recorded in the daemon ledger",
    )
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
