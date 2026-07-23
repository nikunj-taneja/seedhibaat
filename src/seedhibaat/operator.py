from __future__ import annotations

import argparse
import csv
import json
import os
import secrets
import ssl
import re
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


class OperatorError(Exception):
    pass


def _safe_error_detail(value: str) -> str:
    value = re.sub(r"\b(?:EA|shpss_|shpat_|shpca_)[A-Za-z0-9_-]{20,}\b", "[redacted-secret]", value)
    return re.sub(r"\+?\d[\d\s().-]{8,}\d", "[redacted-number]", value)


GENERATED_SECRET_NAMES = (
    "SEEDHIBAAT_API_KEY",
    "SEEDHIBAAT_PII_HASH_KEY",
    "SEEDHIBAAT_LINK_SIGNING_KEY",
    "SEEDHIBAAT_BACKUP_KEY",
    "SEEDHIBAAT_METRICS_PASSWORD",
    "META_WEBHOOK_VERIFY_TOKEN",
)


def run_secrets_init(args: argparse.Namespace) -> int:
    path = args.env_file
    try:
        original = path.read_text(encoding="utf-8") if path.exists() else ""
    except OSError as exc:
        raise OperatorError(f"cannot read environment file {path}: {exc}") from exc
    generated: list[str] = []
    found: set[str] = set()
    output: list[str] = []
    for line in original.splitlines():
        name, separator, value = line.partition("=")
        if separator and name in GENERATED_SECRET_NAMES:
            found.add(name)
            if not value.strip():
                value = secrets.token_urlsafe(48)
                generated.append(name)
            line = name + "=" + value
        output.append(line)
    for name in GENERATED_SECRET_NAMES:
        if name not in found:
            output.append(name + "=" + secrets.token_urlsafe(48))
            generated.append(name)
    body = "\n".join(output) + "\n"
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(prefix=".seedhibaat-env-", dir=path.parent)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                handle.write(body)
                handle.flush()
                os.fsync(handle.fileno())
            os.chmod(temporary, 0o600)
            os.replace(temporary, path)
        except Exception:
            try:
                os.unlink(temporary)
            except OSError:
                pass
            raise
    except OSError as exc:
        raise OperatorError(f"cannot update environment file {path}: {exc}") from exc
    print(f"Environment file secured: {path}")
    print("Generated: " + (", ".join(generated) if generated else "none (existing values preserved)"))
    print("Secret values were not displayed.")
    return 0


def _json_request(
    url: str,
    *,
    method: str = "GET",
    headers: dict[str, str] | None = None,
    payload: Any = None,
    timeout: float = 30,
) -> Any:
    body = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Accept", "application/json")
    if body is not None:
        request.add_header("Content-Type", "application/json")
    for name, value in (headers or {}).items():
        request.add_header(name, value)
    try:
        with urllib.request.urlopen(
            request, timeout=timeout, context=ssl.create_default_context()
        ) as response:
            response_body = response.read()
    except urllib.error.HTTPError as exc:
        detail = _safe_error_detail(exc.read().decode(errors="replace")[:500])
        raise OperatorError(f"HTTP {exc.code}: {detail}") from exc
    except urllib.error.URLError as exc:
        raise OperatorError(f"network error: {exc.reason}") from exc
    if not response_body:
        return None
    try:
        return json.loads(response_body)
    except json.JSONDecodeError as exc:
        raise OperatorError("service returned invalid JSON") from exc


def _required_env(*names: str) -> dict[str, str]:
    values = {name: os.environ.get(name, "").strip() for name in names}
    missing = [name for name, value in values.items() if not value]
    if missing:
        raise OperatorError("missing environment variables: " + ", ".join(missing))
    return values


def daemon_request(path: str, *, method: str = "GET", payload: Any = None) -> Any:
    env = _required_env("SEEDHIBAAT_API_KEY")
    base_url = os.environ.get("SEEDHIBAAT_OPERATOR_URL", "http://127.0.0.1:8088")
    return _json_request(
        base_url.rstrip("/") + path,
        method=method,
        payload=payload,
        headers={"Authorization": "Bearer " + env["SEEDHIBAAT_API_KEY"]},
    )


def run_daemon_status(_: argparse.Namespace) -> int:
    base_url = os.environ.get("SEEDHIBAAT_OPERATOR_URL", "http://127.0.0.1:8088")
    print(json.dumps(_json_request(base_url.rstrip("/") + "/healthz"), indent=2))
    return 0


def run_report(args: argparse.Namespace) -> int:
    query: dict[str, str] = {}
    if args.from_time:
        query["from"] = args.from_time
    if args.to_time:
        query["to"] = args.to_time
    if args.campaign:
        query["campaign"] = args.campaign
    if args.workflow:
        query["workflow"] = args.workflow
    if args.template:
        query["template"] = args.template
    suffix = "?" + urllib.parse.urlencode(query) if query else ""
    print(json.dumps(daemon_request("/api/v1/report" + suffix), indent=2))
    return 0


def run_workflow_list(_: argparse.Namespace) -> int:
    print(json.dumps(daemon_request("/api/v1/workflows"), indent=2))
    return 0


def run_workflow_action(args: argparse.Namespace) -> int:
    path = f"/api/v1/workflows/{urllib.parse.quote(args.name, safe='')}/{args.action}"
    print(json.dumps(daemon_request(path, method="POST", payload={}), indent=2))
    return 0


def run_workflow_preview(args: argparse.Namespace) -> int:
    path = f"/api/v1/workflows/{urllib.parse.quote(args.name, safe='')}/preview"
    print(json.dumps(daemon_request(path), indent=2))
    return 0


def run_workflow_activate(args: argparse.Namespace) -> int:
    if not args.activate or not args.yes:
        raise OperatorError(
            "workflow activation requires --activate --yes and an exact --confirmed-count"
        )
    path = f"/api/v1/workflows/{urllib.parse.quote(args.name, safe='')}/activate"
    payload = {"confirmed_recipient_count": args.confirmed_count}
    print(json.dumps(daemon_request(path, method="POST", payload=payload), indent=2))
    return 0


def run_workflow_validate(args: argparse.Namespace) -> int:
    try:
        body = args.file.read_text(encoding="utf-8")
    except OSError as exc:
        raise OperatorError(f"cannot read workflow {args.file}: {exc}") from exc
    print(json.dumps(daemon_request("/api/v1/workflows/validate", method="POST", payload={"yaml": body}), indent=2))
    return 0


def run_workflow_simulate(args: argparse.Namespace) -> int:
    try:
        body = args.file.read_text(encoding="utf-8")
    except OSError as exc:
        raise OperatorError(f"cannot read workflow {args.file}: {exc}") from exc
    payload = {"yaml": body, "triggered_at": args.triggered_at}
    if args.as_of:
        payload["as_of"] = args.as_of
    print(
        json.dumps(
            daemon_request(
                "/api/v1/workflows/simulate", method="POST", payload=payload
            ),
            indent=2,
        )
    )
    return 0


def run_workflow_reload(_: argparse.Namespace) -> int:
    print(json.dumps(daemon_request("/api/v1/workflows/reload", method="POST", payload={}), indent=2))
    return 0


def run_run_action(args: argparse.Namespace) -> int:
    path = f"/api/v1/runs/{urllib.parse.quote(args.run_id, safe='')}/{args.action}"
    print(json.dumps(daemon_request(path, method="POST", payload={}), indent=2))
    return 0


def run_run_cancel(args: argparse.Namespace) -> int:
    if not args.cancel or not args.yes:
        raise OperatorError("workflow-run cancellation requires --cancel --yes")
    path = f"/api/v1/runs/{urllib.parse.quote(args.run_id, safe='')}/cancel"
    print(json.dumps(daemon_request(path, method="POST", payload={}), indent=2))
    return 0


def run_job_replay(args: argparse.Namespace) -> int:
    if not args.replay or not args.yes:
        raise OperatorError("failed-job replay requires --replay --yes")
    path = f"/api/v1/jobs/{urllib.parse.quote(args.job_id, safe='')}/replay"
    print(json.dumps(daemon_request(path, method="POST", payload={}), indent=2))
    return 0


def run_audit(args: argparse.Namespace) -> int:
    print(json.dumps(daemon_request("/api/v1/audit?" + urllib.parse.urlencode({"limit": args.limit})), indent=2))
    return 0


def run_integrity(_: argparse.Namespace) -> int:
    print(json.dumps(daemon_request("/api/v1/integrity", method="POST", payload={}), indent=2))
    return 0


def _meta_request(path: str, *, method: str = "GET", payload: Any = None, query: dict[str, str] | None = None) -> Any:
    env = _required_env("META_SYSTEM_USER_TOKEN", "META_GRAPH_API_VERSION")
    url = f"https://graph.facebook.com/{env['META_GRAPH_API_VERSION'].strip('/')}/{path.lstrip('/')}"
    if query:
        url += "?" + urllib.parse.urlencode(query)
    return _json_request(
        url,
        method=method,
        payload=payload,
        headers={"Authorization": "Bearer " + env["META_SYSTEM_USER_TOKEN"]},
    )


def run_meta_identity(args: argparse.Namespace) -> int:
    if args.profile == "test":
        env = _required_env(
            "WHATSAPP_TEST_PHONE_NUMBER_ID", "WHATSAPP_TEST_BUSINESS_ACCOUNT_ID"
        )
        phone_id = env["WHATSAPP_TEST_PHONE_NUMBER_ID"]
        waba_id = env["WHATSAPP_TEST_BUSINESS_ACCOUNT_ID"]
    else:
        env = _required_env("WHATSAPP_PHONE_NUMBER_ID", "WHATSAPP_BUSINESS_ACCOUNT_ID")
        phone_id = env["WHATSAPP_PHONE_NUMBER_ID"]
        waba_id = env["WHATSAPP_BUSINESS_ACCOUNT_ID"]
    phone = _meta_request(
        phone_id,
        query={"fields": "id,display_phone_number,verified_name,quality_rating,code_verification_status"},
    )
    phones = _meta_request(
        waba_id + "/phone_numbers",
        query={"fields": "id,display_phone_number,verified_name,quality_rating,code_verification_status"},
    )
    belongs = any(item.get("id") == phone.get("id") for item in phones.get("data", []))
    safe = {
        "profile": args.profile,
        "phone_number_id": phone.get("id"),
        "verified_name": phone.get("verified_name"),
        "quality_rating": phone.get("quality_rating"),
        "code_verification_status": phone.get("code_verification_status"),
        "belongs_to_configured_waba": belongs,
    }
    print(json.dumps(safe, indent=2))
    return 0 if belongs else 1


def _load_template(path: Path) -> dict[str, Any]:
    try:
        template = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise OperatorError(f"cannot read template definition {path}: {exc}") from exc
    required = {"name", "language", "category", "components"}
    missing = sorted(required - template.keys())
    if missing:
        raise OperatorError("template definition missing: " + ", ".join(missing))
    if template["category"] not in {"MARKETING", "UTILITY"}:
        raise OperatorError("template category must be MARKETING or UTILITY")
    return template


def run_template_submit(args: argparse.Namespace) -> int:
    template = _load_template(args.file)
    print(f"Template: {template['name']} ({template['language']})")
    print(f"Category: {template['category']}")
    for component in template["components"]:
        component_type = str(component.get("type", "")).upper()
        if component_type in {"HEADER", "BODY", "FOOTER"} and component.get("text"):
            print(f"{component_type.title()}: {component['text']}")
        if component_type == "BUTTONS":
            for button in component.get("buttons", []):
                summary = f"Button: {button.get('type', '')} — {button.get('text', '')}"
                if button.get("url"):
                    summary += f" — {button['url']}"
                print(summary)
    if not args.submit:
        print("Dry run only. Add --submit --yes to submit this exact definition to Meta.")
        return 0
    if not args.yes:
        raise OperatorError("non-interactive submission requires --submit --yes")
    env = _required_env("WHATSAPP_BUSINESS_ACCOUNT_ID")
    result = _meta_request(
        env["WHATSAPP_BUSINESS_ACCOUNT_ID"] + "/message_templates",
        method="POST",
        payload=template,
    )
    print(json.dumps({"submitted": template["name"], "meta_template_id": result.get("id")}, indent=2))
    return 0


def run_template_status(args: argparse.Namespace) -> int:
    env = _required_env("WHATSAPP_BUSINESS_ACCOUNT_ID")
    result = _meta_request(
        env["WHATSAPP_BUSINESS_ACCOUNT_ID"] + "/message_templates",
        query={"fields": "id,name,status,category,language", "name": args.name, "limit": "100"},
    )
    safe = [
        {key: item.get(key) for key in ("id", "name", "status", "category", "language")}
        for item in result.get("data", [])
    ]
    print(json.dumps({"templates": safe}, indent=2))
    return 0


def _shopify_token() -> tuple[str, dict[str, str]]:
    env = _required_env(
        "SHOPIFY_SHOP_DOMAIN", "SHOPIFY_CLIENT_ID", "SHOPIFY_CLIENT_SECRET", "SHOPIFY_API_VERSION"
    )
    token = _json_request(
        f"https://{env['SHOPIFY_SHOP_DOMAIN']}/admin/oauth/access_token",
        method="POST",
        payload={
            "grant_type": "client_credentials",
            "client_id": env["SHOPIFY_CLIENT_ID"],
            "client_secret": env["SHOPIFY_CLIENT_SECRET"],
        },
    )
    if not token.get("access_token"):
        raise OperatorError("Shopify returned no access token")
    return token["access_token"], env


def run_shopify_scopes(_: argparse.Namespace) -> int:
    token, env = _shopify_token()
    result = _json_request(
        f"https://{env['SHOPIFY_SHOP_DOMAIN']}/admin/api/{env['SHOPIFY_API_VERSION']}/graphql.json",
        method="POST",
        headers={"X-Shopify-Access-Token": token},
        payload={"query": "query { currentAppInstallation { accessScopes { handle } } }"},
    )
    if result.get("errors"):
        raise OperatorError("Shopify GraphQL rejected the scope query")
    granted = sorted(
        item["handle"]
        for item in result["data"]["currentAppInstallation"]["accessScopes"]
    )
    required = sorted(
        {
            "read_orders",
            "read_all_orders",
            "read_customers",
            "read_products",
            "read_inventory",
            "read_locations",
            "read_returns",
        }
    )
    print(json.dumps({"granted": granted, "missing": sorted(set(required) - set(granted))}, indent=2))
    return 1 if set(required) - set(granted) else 0


SHOPIFY_WEBHOOK_TOPICS = (
    "ORDERS_CREATE",
    "ORDERS_UPDATED",
    "ORDERS_CANCELLED",
    "ORDERS_FULFILLED",
    "ORDERS_PARTIALLY_FULFILLED",
    "FULFILLMENTS_CREATE",
    "FULFILLMENTS_UPDATE",
    "REFUNDS_CREATE",
    "RETURNS_REQUEST",
    "RETURNS_APPROVE",
    "RETURNS_UPDATE",
    "RETURNS_PROCESS",
    "RETURNS_CANCEL",
    "RETURNS_CLOSE",
    "CUSTOMERS_CREATE",
    "CUSTOMERS_DELETE",
    "CUSTOMERS_UPDATE",
    "CUSTOMERS_MARKETING_CONSENT_UPDATE",
    "CUSTOMERS_WHATS_APP_MARKETING_CONSENT_UPDATE",
    "PRODUCTS_UPDATE",
    "INVENTORY_LEVELS_UPDATE",
    "APP_UNINSTALLED",
)


def _shopify_graphql(token: str, env: dict[str, str], query: str, variables: dict[str, Any] | None = None) -> Any:
    result = _json_request(
        f"https://{env['SHOPIFY_SHOP_DOMAIN']}/admin/api/{env['SHOPIFY_API_VERSION']}/graphql.json",
        method="POST",
        headers={"X-Shopify-Access-Token": token},
        payload={"query": query, "variables": variables or {}},
    )
    if result.get("errors"):
        raise OperatorError("Shopify GraphQL rejected the request: " + result["errors"][0].get("message", "unknown error"))
    return result.get("data", {})


def run_shopify_webhooks(args: argparse.Namespace) -> int:
    callback = args.callback.rstrip("/")
    if not callback.startswith("https://"):
        raise OperatorError("Shopify webhook callback must use HTTPS")
    token, env = _shopify_token()
    query = "query { webhookSubscriptions(first: 250) { nodes { id topic uri } } }"
    data = _shopify_graphql(token, env, query)
    existing = {
        (item.get("topic"), item.get("uri"))
        for item in data["webhookSubscriptions"]["nodes"]
    }
    missing = [topic for topic in SHOPIFY_WEBHOOK_TOPICS if (topic, callback) not in existing]
    print(json.dumps({"callback": callback, "required_topics": list(SHOPIFY_WEBHOOK_TOPICS), "missing_topics": missing}, indent=2))
    if not args.register:
        print("Dry run only. Add --register --yes to create the missing subscriptions.")
        return 0
    if not args.yes:
        raise OperatorError("webhook registration requires --register --yes")
    mutation = """mutation CreateWebhook($topic: WebhookSubscriptionTopic!, $subscription: WebhookSubscriptionInput!) {
      webhookSubscriptionCreate(topic: $topic, webhookSubscription: $subscription) {
        webhookSubscription { id topic uri }
        userErrors { field message }
      }
    }"""
    created: list[dict[str, Any]] = []
    for topic in missing:
        result = _shopify_graphql(
            token,
            env,
            mutation,
            {"topic": topic, "subscription": {"uri": callback}},
        )["webhookSubscriptionCreate"]
        if result.get("userErrors"):
            raise OperatorError(f"Shopify rejected {topic}: {result['userErrors'][0]['message']}")
        created.append(result["webhookSubscription"])
    print(json.dumps({"created": created, "already_present": len(SHOPIFY_WEBHOOK_TOPICS) - len(missing)}, indent=2))
    return 0


def _segment_payload(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "kind": args.kind,
        "product_handle": args.product_handle or "",
        "product_title": args.product_title or "",
        "within_days": args.within_days or 0,
        "lapsed_days": args.lapsed_days or 0,
        "require_whatsapp_consent": not args.allow_unknown_consent,
        "exclude_product_handle": args.exclude_product_handle or "",
        "exclude_product_title": args.exclude_product_title or "",
        "exclude_product_tag": args.exclude_product_tag or "",
        "exclude_recent_purchase_days": args.exclude_recent_days or 0,
    }


def run_segment_preview(args: argparse.Namespace) -> int:
    result = daemon_request("/api/v1/segments/preview", method="POST", payload=_segment_payload(args))
    print(json.dumps(result, indent=2))
    return 0


def run_campaign_create(args: argparse.Namespace) -> int:
    payload = {
        "name": args.name,
        "segment": _segment_payload(args),
        "template": args.template,
        "language": args.language,
        "tracked_url": args.tracked_url or "",
        "scheduled_at": args.scheduled_at or "",
        "frequency_messages": args.frequency_messages,
        "frequency_window": args.frequency_window,
    }
    result = daemon_request("/api/v1/campaigns", method="POST", payload=payload)
    print(json.dumps(result, indent=2))
    print("Draft only. No messages were queued.")
    return 0


def run_campaign_show(args: argparse.Namespace) -> int:
    print(json.dumps(daemon_request(f"/api/v1/campaigns/{urllib.parse.quote(args.campaign_id, safe='')}"), indent=2))
    return 0


def run_campaign_activate(args: argparse.Namespace) -> int:
    if not args.activate or not args.yes:
        raise OperatorError("campaign activation requires --activate --yes and an exact --confirmed-count")
    payload = {
        "confirmed_recipient_count": args.confirmed_count,
        "name": "",
        "segment": {"kind": "not_reordered", "require_whatsapp_consent": True},
        "template": "",
        "language": "",
    }
    result = daemon_request(
        f"/api/v1/campaigns/{urllib.parse.quote(args.campaign_id, safe='')}/activate",
        method="POST",
        payload=payload,
    )
    print(json.dumps(result, indent=2))
    return 0


def run_campaign_cancel(args: argparse.Namespace) -> int:
    if not args.cancel or not args.yes:
        raise OperatorError("campaign cancellation requires --cancel --yes")
    path = f"/api/v1/campaigns/{urllib.parse.quote(args.campaign_id, safe='')}/cancel"
    print(json.dumps(daemon_request(path, method="POST", payload={}), indent=2))
    return 0


def _normalize_consent_phone(value: str, default_country_code: str = "") -> str:
    digits = re.sub(r"[\s()+.\-]", "", value.strip())
    raw = value.strip()
    if raw.startswith("+"):
        pass
    elif digits.startswith("00"):
        digits = digits[2:]
    elif default_country_code:
        country = default_country_code.strip().lstrip("+")
        if not country.isdigit() or country.startswith("0"):
            raise OperatorError("default country code must contain digits without a leading zero")
        if not digits.startswith(country):
            digits = country + digits
    if not re.fullmatch(r"[1-9]\d{7,14}", digits):
        raise OperatorError("consent CSV contains an invalid E.164 phone number")
    return digits


def _read_consent_csv(path: Path, default_country_code: str = "") -> list[dict[str, str]]:
    try:
        handle = path.open(newline="", encoding="utf-8-sig")
    except OSError as exc:
        raise OperatorError(f"cannot read consent CSV: {exc}") from exc
    with handle:
        reader = csv.DictReader(handle)
        required = {"phone", "consent"}
        if not reader.fieldnames or not required.issubset(reader.fieldnames):
            raise OperatorError("consent CSV requires phone and consent columns")
        records = []
        for row in reader:
            consent = (row.get("consent") or "").strip().lower()
            if consent not in {"opted_in", "opted_out"}:
                raise OperatorError("consent values must be opted_in or opted_out")
            records.append({"phone": _normalize_consent_phone(row.get("phone") or "", default_country_code), "consent": consent, "consent_at": (row.get("consent_at") or "").strip()})
    if not records:
        raise OperatorError("consent CSV contains no records")
    return records


def run_consent_import(args: argparse.Namespace) -> int:
    records = _read_consent_csv(
        args.csv,
        args.default_country_code
        or os.environ.get("SEEDHIBAAT_DEFAULT_COUNTRY_CODE", ""),
    )
    unique = {(item["phone"], item["consent"], item["consent_at"]) for item in records}
    print(f"Validated consent records: {len(records)}")
    print(f"Unique consent records: {len(unique)}")
    if not args.import_records:
        print("Dry run only. Add --import --yes to update matching customers.")
        return 0
    if not args.yes:
        raise OperatorError("consent import requires --import --yes")
    result = daemon_request("/api/v1/consent/import", method="POST", payload={"source": args.source, "records": records})
    print(json.dumps(result, indent=2))
    return 0


def _add_segment_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--kind", required=True, choices=("product_buyers", "not_reordered", "recent_purchasers", "lapsed_customers", "back_in_stock"))
    parser.add_argument("--product-handle")
    parser.add_argument("--product-title")
    parser.add_argument("--within-days", type=int)
    parser.add_argument("--lapsed-days", type=int)
    parser.add_argument("--exclude-recent-days", type=int)
    parser.add_argument("--exclude-product-handle")
    parser.add_argument("--exclude-product-title")
    parser.add_argument("--exclude-product-tag")
    parser.add_argument("--allow-unknown-consent", action="store_true", help="preview only; activation still applies suppression rules")


def register_operator_commands(subparsers: argparse._SubParsersAction) -> None:
    secrets_parser = subparsers.add_parser("secrets", help="generate local non-provider secrets safely")
    secrets_sub = secrets_parser.add_subparsers(dest="secrets_command", required=True)
    secrets_init = secrets_sub.add_parser("init")
    secrets_init.add_argument("--env-file", type=Path, default=Path(".env"))
    secrets_init.set_defaults(func=run_secrets_init)

    daemon = subparsers.add_parser("daemon", help="inspect the local SeedhiBaat service")
    daemon_sub = daemon.add_subparsers(dest="daemon_command", required=True)
    status = daemon_sub.add_parser("status")
    status.set_defaults(func=run_daemon_status)

    report = subparsers.add_parser("report", help="show aggregate delivery and attribution metrics")
    report.add_argument("--from", dest="from_time")
    report.add_argument("--to", dest="to_time")
    report.add_argument("--campaign")
    report.add_argument("--workflow")
    report.add_argument("--template")
    report.set_defaults(func=run_report)

    workflow_parser = subparsers.add_parser("workflow", help="inspect or control workflows")
    workflow_sub = workflow_parser.add_subparsers(dest="workflow_command", required=True)
    workflow_list = workflow_sub.add_parser("list")
    workflow_list.set_defaults(func=run_workflow_list)
    workflow_validate = workflow_sub.add_parser("validate")
    workflow_validate.add_argument("--file", type=Path, required=True)
    workflow_validate.set_defaults(func=run_workflow_validate)
    workflow_simulate = workflow_sub.add_parser(
        "simulate", help="calculate a workflow schedule without writing or sending"
    )
    workflow_simulate.add_argument("--file", type=Path, required=True)
    workflow_simulate.add_argument("--triggered-at", required=True)
    workflow_simulate.add_argument("--as-of")
    workflow_simulate.set_defaults(func=run_workflow_simulate)
    workflow_reload = workflow_sub.add_parser("reload")
    workflow_reload.set_defaults(func=run_workflow_reload)
    workflow_preview = workflow_sub.add_parser("preview")
    workflow_preview.add_argument("name")
    workflow_preview.set_defaults(func=run_workflow_preview)
    workflow_activate = workflow_sub.add_parser("activate")
    workflow_activate.add_argument("name")
    workflow_activate.add_argument("--confirmed-count", type=int, required=True)
    workflow_activate.add_argument("--activate", action="store_true")
    workflow_activate.add_argument("--yes", action="store_true")
    workflow_activate.set_defaults(func=run_workflow_activate)
    workflow_pause = workflow_sub.add_parser("pause")
    workflow_pause.add_argument("name")
    workflow_pause.set_defaults(func=run_workflow_action, action="pause")

    run_parser = subparsers.add_parser("run", help="control a persisted workflow run")
    run_sub = run_parser.add_subparsers(dest="run_command", required=True)
    for action in ("pause", "resume"):
        command = run_sub.add_parser(action)
        command.add_argument("run_id")
        command.set_defaults(func=run_run_action, action=action)
    run_cancel = run_sub.add_parser("cancel")
    run_cancel.add_argument("run_id")
    run_cancel.add_argument("--cancel", action="store_true")
    run_cancel.add_argument("--yes", action="store_true")
    run_cancel.set_defaults(func=run_run_cancel)

    job_parser = subparsers.add_parser("job", help="inspect or replay durable jobs")
    job_sub = job_parser.add_subparsers(dest="job_command", required=True)
    job_replay = job_sub.add_parser("replay")
    job_replay.add_argument("job_id")
    job_replay.add_argument("--replay", action="store_true")
    job_replay.add_argument("--yes", action="store_true")
    job_replay.set_defaults(func=run_job_replay)

    audit = subparsers.add_parser("audit", help="show privacy-safe operator audit history")
    audit.add_argument("--limit", type=int, default=100)
    audit.set_defaults(func=run_audit)

    integrity = subparsers.add_parser("integrity", help="run SQLite integrity checks")
    integrity.set_defaults(func=run_integrity)

    meta_parser = subparsers.add_parser("meta", help="inspect configured Meta assets")
    meta_sub = meta_parser.add_subparsers(dest="meta_command", required=True)
    identity = meta_sub.add_parser("identity")
    identity.add_argument("--profile", choices=("active", "test"), default="active")
    identity.set_defaults(func=run_meta_identity)

    template_parser = subparsers.add_parser("template", help="submit and inspect Meta templates")
    template_sub = template_parser.add_subparsers(dest="template_command", required=True)
    submit = template_sub.add_parser("submit")
    submit.add_argument("--file", type=Path, required=True)
    submit.add_argument("--submit", action="store_true")
    submit.add_argument("--yes", action="store_true")
    submit.set_defaults(func=run_template_submit)
    template_status = template_sub.add_parser("status")
    template_status.add_argument("name")
    template_status.set_defaults(func=run_template_status)

    shopify_parser = subparsers.add_parser("shopify", help="inspect Shopify integration")
    shopify_sub = shopify_parser.add_subparsers(dest="shopify_command", required=True)
    scopes = shopify_sub.add_parser("scopes")
    scopes.set_defaults(func=run_shopify_scopes)
    webhooks = shopify_sub.add_parser("webhooks")
    webhooks.add_argument("--callback", required=True)
    webhooks.add_argument("--register", action="store_true")
    webhooks.add_argument("--yes", action="store_true")
    webhooks.set_defaults(func=run_shopify_webhooks)

    segment_parser = subparsers.add_parser("segment", help="preview privacy-safe customer segments")
    segment_sub = segment_parser.add_subparsers(dest="segment_command", required=True)
    segment_preview = segment_sub.add_parser("preview")
    _add_segment_arguments(segment_preview)
    segment_preview.set_defaults(func=run_segment_preview)

    campaign_parser = subparsers.add_parser("campaign", help="draft, inspect, and explicitly activate one-off campaigns")
    campaign_sub = campaign_parser.add_subparsers(dest="campaign_command", required=True)
    campaign_create = campaign_sub.add_parser("create")
    campaign_create.add_argument("--name", required=True)
    campaign_create.add_argument("--template", required=True)
    campaign_create.add_argument("--language", default="en_US")
    campaign_create.add_argument("--tracked-url")
    campaign_create.add_argument("--scheduled-at")
    campaign_create.add_argument("--frequency-messages", type=int, default=1)
    campaign_create.add_argument("--frequency-window", default="24h")
    _add_segment_arguments(campaign_create)
    campaign_create.set_defaults(func=run_campaign_create)
    campaign_show = campaign_sub.add_parser("show")
    campaign_show.add_argument("campaign_id")
    campaign_show.set_defaults(func=run_campaign_show)
    campaign_activate = campaign_sub.add_parser("activate")
    campaign_activate.add_argument("campaign_id")
    campaign_activate.add_argument("--confirmed-count", type=int, required=True)
    campaign_activate.add_argument("--activate", action="store_true")
    campaign_activate.add_argument("--yes", action="store_true")
    campaign_activate.set_defaults(func=run_campaign_activate)
    campaign_cancel = campaign_sub.add_parser("cancel")
    campaign_cancel.add_argument("campaign_id")
    campaign_cancel.add_argument("--cancel", action="store_true")
    campaign_cancel.add_argument("--yes", action="store_true")
    campaign_cancel.set_defaults(func=run_campaign_cancel)

    consent_parser = subparsers.add_parser("consent", help="validate or import WhatsApp consent records")
    consent_sub = consent_parser.add_subparsers(dest="consent_command", required=True)
    consent_import = consent_sub.add_parser("import")
    consent_import.add_argument("--csv", type=Path, required=True)
    consent_import.add_argument("--source", required=True)
    consent_import.add_argument("--default-country-code")
    consent_import.add_argument("--import", dest="import_records", action="store_true")
    consent_import.add_argument("--yes", action="store_true")
    consent_import.set_defaults(func=run_consent_import)
