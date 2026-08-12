# SeedhiBaat

SeedhiBaat is a self-hosted WhatsApp marketing automation system. Its Python
CLI is the operator interface; its Go daemon receives Meta and Shopify
webhooks, runs durable YAML workflows, stores state in SQLite, and can serve an
optional read-only aggregate metrics dashboard.

## Operating rules

- `AGENTS.md` is the canonical project instruction file.
- Use the tracked `.sbconfig.example/` bundle to understand the shape of a
  deployment. Copy it to the ignored `.sbconfig/` directory before adding any
  real business identifiers, templates, workflows, or migration notes.
- Never put a real deployment in `.sbconfig.example/`; it is public,
  placeholder-only documentation.
- Never place access tokens, app secrets, customer data, signing keys, or
  verification tokens in source, examples, commands committed to Git, logs,
  test fixtures, or chat responses.
- Treat every message operation as a dry run unless the user explicitly asks
  for a live send.
- Before every live send, state the exact template, validated message count,
  and unique recipient count. Require explicit approval for those values.
- Before every campaign activation, workflow activation, live test, resend, or
  replay, validate the complete rendered message contract, not only the
  template name and audience. Confirm the approved template exists on the
  configured WABA in the intended language/category; every required header
  media value and ordered body/header parameter is present; every dynamic URL
  button has a parameter; every tracked link has the approved final HTTPS
  destination; and all externally fetched media and destinations return a
  successful response. Compare the frozen job payload shape with the approved
  definition before queueing. If any required field is absent or differs, do
  not activate or replay.
- A retry or replay must preserve the exact approved render contract from the
  original intended send, including media, parameters, button URL shape, and
  tracked destination. Reconcile and repair pre-acceptance failed payloads
  before replay; never infer missing fields from the template name alone.
- Never add `--send --yes`, `--submit --yes`, campaign activation, production
  workflow activation, or `--allow-resend` based on implied intent.
- Never submit a template to Meta until the exact name, language, category,
  body, buttons, and URL shape have been shown to the user and the user has
  explicitly approved that exact definition for submission.
- The only authorized live test recipient is the number designated by the
  repository owner. Never copy that number into tracked files or fixtures.
- Never activate a customer-facing campaign or always-on production workflow
  until its audience definition, exclusions, templates, timing, frequency cap,
  and frozen recipient count have been shown to and explicitly approved by the
  user.
- Preserve `state/sends.ndjson` and the SQLite database. Both protect against
  duplicate sends.
- Meta API acceptance is only `accepted`; never describe it as sent,
  delivered, or read without the corresponding verified webhook.
- Marketing use cases must be submitted as `MARKETING`. Do not attempt to
  influence or bypass Meta classification.
- Keep both production gates off by default:
  `SEEDHIBAAT_OUTBOUND_SENDING_ENABLED=false` and
  `SEEDHIBAAT_PRODUCTION_FLOW_ENABLED=false`.
- Preview and report commands must never display phone numbers, decrypted PII,
  or rendered private template parameters.
- The metrics dashboard is aggregate and read-only. Never add customer PII or
  state-changing campaign, workflow, template, replay, or send controls to it;
  the Python CLI remains the sole operator interface.
- Preserve unrelated host services. SeedhiBaat owns only its dedicated service,
  user, directories, loopback port, and reverse-proxy virtual host.
- Preserve the verified test WABA/phone profile separately from the active
  profile until production identity and access have been proven.
- Manual replay is limited to failed jobs. Never replay an accepted or unknown
  Meta outcome; reconcile it first to avoid a duplicate.

## Development

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
go test -race -timeout 90s ./...
go vet ./...
python3 tools/privacy_scan.py
git diff --check
```

Run the no-network load test with:

```bash
go run ./cmd/seedhibaat-loadtest 10000
```

See `docs/runbook.md` for operator and production procedures.
