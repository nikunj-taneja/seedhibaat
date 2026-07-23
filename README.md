# SeedhiBaat

SeedhiBaat is a self-hosted alternative to hosted WhatsApp automation
platforms. Technical operators connect their own Meta WhatsApp Cloud API app
and, optionally, their own Shopify app. SeedhiBaat provides the durable queue,
webhooks, workflows, segmentation, consent enforcement, click tracking,
attribution, and command-line operations.

It is not a Meta Business Solution Provider and does not bypass Meta template
review, conversation billing, consent rules, or messaging policies.

There is no state-changing browser UI. A Python CLI is the operator interface;
a Go daemon receives webhooks, runs durable work from SQLite, and optionally
serves an authenticated, read-only aggregate metrics dashboard.

## Capabilities

- Submit explicitly approved WhatsApp template definitions and poll their Meta
  review status
- Send approved templates from CSV with variables and static or dynamic URL
  buttons
- Run versioned, event-driven YAML workflows with durable waits
- Build product-buyer, recent-purchaser, lapsed, not-reordered, and
  back-in-stock audiences
- Synchronize Shopify orders, customers, products, inventory, fulfillments,
  refunds, returns, cancellations, and WhatsApp consent
- Suppress future work after opt-out, invalid number, refund, return,
  cancellation, or a configured conversion
- Track accepted, sent, delivered, observed-read, failed, reply, click,
  opt-out, conversion, and attributed revenue events
- Generate signed per-recipient redirect links for exact unique-click
  attribution
- Run with SQLite WAL, migrations, integrity checks, encrypted backups,
  structured logs, Nginx, systemd, and no external queue service
- View an optional read-only dashboard for delivery, observed reads, unique
  CTR, conversions, and currency-safe attributed revenue

Production sending and production workflow activation are independent gates
and both default to off. Meta API acceptance is never reported as delivery.

See [architecture](docs/architecture.md),
[configuration](docs/configuration.md), and the
[production runbook](docs/runbook.md).

## Safety model

- Store credentials only in an ignored mode-`0600` `.env` or an owner-only
  server environment file.
- Never commit customer data, tokens, app secrets, signing keys, local
  deployment configuration, or rendered message parameters.
- Treat every send as a dry run until the exact template, validated message
  count, and unique recipient count have been explicitly approved.
- Never submit a template to Meta until its exact name, language, category,
  body, buttons, and URL shape have been shown to and approved by the operator.
- Never activate an audience or workflow until its definition, exclusions,
  templates, schedule, frequency cap, and frozen recipient count are approved.
- Use only recipients with valid WhatsApp consent. STOP and equivalent
  opt-outs cancel pending work immediately.
- Marketing nudges, abandoned-checkout recovery, back-in-stock alerts,
  repurchase prompts, and review requests should be treated as `MARKETING`.
  Meta makes the final category and approval decision.

## Architecture

```text
Shopify webhooks ─┐
                  ├─> reverse proxy ─> signature check ─> durable inbox
Meta webhooks ────┘                                      │
                                                         v
Shopify GraphQL <─ reconciliation worker <─ SQLite ─> scheduler
                                                         │
                                                         v
Meta Cloud API <──────── bounded outbound workers <──── queue
                                                         │
Recipient click ─> signed /r/ redirect ─> click event ───┘

Python CLI ─> authenticated loopback JSON API ─> previews and controls
```

## Requirements

- Python 3.10+
- Go 1.26+ for daemon development
- A Linux server with a public HTTPS hostname
- A Meta business portfolio, developer app, WABA, and Cloud API phone number
- Optionally, a Shopify app with Admin GraphQL access

## Meta setup

Meta uses several surfaces:

- [Meta for Developers](https://developers.facebook.com/) creates the app.
- [Meta Business Settings](https://business.facebook.com/settings/) connects
  the portfolio, app, system user, and WhatsApp assets.
- [WhatsApp Manager](https://business.facebook.com/wa/manage/home/) manages
  WABAs, phone numbers, display names, and templates.

### Create and connect the app

1. Sign into Meta for Developers with a profile that controls the intended
   business portfolio.
2. Create a Business app and add the WhatsApp product.
3. Connect the app to the same portfolio that owns the WABA.
4. In Business Settings, create a system user.
5. Assign the app and WABA to that system user.
6. Generate a non-expiring system-user token with:

```text
whatsapp_business_management
whatsapp_business_messaging
whatsapp_business_manage_events
```

Record the app ID, app secret, WABA ID, phone-number ID, and system-user token.
The visible telephone number is not the phone-number ID.

Keep a verified test WABA/phone pair in the separate
`WHATSAPP_TEST_*` variables while production access is being checked:

```bash
PYTHONPATH=src python3 -m seedhibaat meta identity --profile test
PYTHONPATH=src python3 -m seedhibaat meta identity --profile active
```

### Meta webhooks

Once HTTPS is live, configure:

```text
https://wa.example.com/webhooks/meta
```

Use the independently generated `META_WEBHOOK_VERIFY_TOKEN`, subscribe the
`whatsapp_business_account` object to `messages`, and subscribe each WABA to
the app. SeedhiBaat verifies `X-Hub-Signature-256` using the app secret.

### Phone verification and registration

A production number must be ownership-verified and registered for Cloud API.
Meta's official flow is:

1. Request an SMS or voice verification code.
2. Submit the received code.
3. Register the phone-number ID while setting a six-digit two-step-verification
   PIN.

Never commit or paste the verification code or PIN. Operators can perform
these calls through Meta's official WhatsApp Cloud API collection or add
equivalent commands to a private deployment runbook.

## Shopify setup

Create and install a Shopify app through the current Dev Dashboard. SeedhiBaat
uses the server-to-server client-credentials grant and obtains short-lived
Admin API access tokens automatically.

### Core scopes

```text
read_orders
read_all_orders
read_customers
read_products
read_inventory
read_locations
read_returns
```

`read_all_orders` is needed for history beyond Shopify's normal order window
and may require separate approval. Request protected customer-data access only
for the customer phone, name, and customer/order association that the
automation actually uses.

Check the installed grant without displaying credentials or customer data:

```bash
PYTHONPATH=src python3 -m seedhibaat shopify scopes
```

### Abandoned checkouts

Shopify's current GraphQL `abandonedCheckouts` query requires `read_orders`,
which is already part of the core set. Access also requires protected customer
data and the relevant `manage_abandoned_checkouts` merchant permission.
SeedhiBaat must still be configured or extended with an abandoned-checkout
reconciliation/trigger before such a flow can run; a scope alone does not
create the automation.

Recovery must stop when the checkout is recovered, the customer opts out, the
cart becomes invalid, or the configured frequency cap is reached.

### Shopify webhooks

Register operational subscriptions after HTTPS is ready:

```bash
PYTHONPATH=src python3 -m seedhibaat shopify webhooks \
  --callback https://wa.example.com/webhooks/shopify
```

Review the dry run, then use `--register --yes` only for the reviewed topics.
SeedhiBaat verifies `X-Shopify-Hmac-Sha256`, validates the shop domain, and
deduplicates `X-Shopify-Webhook-Id`.

Configure Shopify's mandatory compliance topics in the app configuration:

```toml
[webhooks]
api_version = "2026-07"

[[webhooks.subscriptions]]
compliance_topics = ["customers/data_request", "customers/redact", "shop/redact"]
uri = "https://wa.example.com/webhooks/shopify"
```

Deploy/release the updated app configuration. These compliance subscriptions
are not created by SeedhiBaat's ordinary Admin GraphQL webhook command.

### WhatsApp consent

SMS or email consent is not WhatsApp consent. SeedhiBaat uses Shopify's native
WhatsApp marketing-consent state when available. `SUBSCRIBED` opts in;
`UNSUBSCRIBED` and `REDACTED` opt out; pending, absent, or unknown states fail
closed. Explicit import is available for consent collected by a previous
provider.

## Local configuration

```bash
git clone https://github.com/nikunj-taneja/seedhibaat.git
cd seedhibaat
cp .env.example .env
chmod 600 .env
git check-ignore .env
PYTHONPATH=src python3 -m seedhibaat secrets init --env-file .env
```

Fill provider values locally. Set:

```dotenv
SEEDHIBAAT_PUBLIC_BASE_URL=https://wa.example.com
SEEDHIBAAT_REDIRECT_ALLOWED_HOSTS=example.com,www.example.com
SEEDHIBAAT_OUTBOUND_SENDING_ENABLED=false
SEEDHIBAAT_PRODUCTION_FLOW_ENABLED=false
```

The redirect allowlist contains storefront hosts that tracked links may open.
It must not contain schemes, paths, or wildcard hosts.

## Templates

Template files are operator-owned and should normally live under the ignored
`.sbconfig/templates/` directory. SeedhiBaat intentionally ships no
business-ready template copy.

Validation is local:

```bash
PYTHONPATH=src python3 -m seedhibaat template submit \
  --file .sbconfig/templates/example.json
```

Before submission, review and explicitly approve the exact name, language,
category, body, buttons, and URL shape shown by the dry run. Only then repeat
the same file with:

```bash
PYTHONPATH=src python3 -m seedhibaat template submit \
  --file .sbconfig/templates/example.json \
  --submit --yes
```

Submission is not approval. Poll the exact template name before sending.

For tracked URL buttons, the template uses the public SeedhiBaat redirect URL
with a dynamic suffix, for example
`https://wa.example.com/r/{{1}}`. SeedhiBaat supplies a unique signed suffix
and redirects only to `SEEDHIBAAT_REDIRECT_ALLOWED_HOSTS`.

## Workflows

Tracked YAML files are disabled examples, not production campaigns. Copy one
into `.sbconfig/workflows/` and configure product selectors, templates,
timezone, quiet hours, delays, conversion rules, and tracked destinations.

```bash
PYTHONPATH=src python3 -m seedhibaat workflow validate \
  --file .sbconfig/workflows/post_delivery.yaml
PYTHONPATH=src python3 -m seedhibaat workflow reload
PYTHONPATH=src python3 -m seedhibaat workflow list
PYTHONPATH=src python3 -m seedhibaat workflow preview post_delivery
PYTHONPATH=src python3 -m seedhibaat workflow simulate \
  --file .sbconfig/workflows/post_delivery.yaml \
  --triggered-at 2026-07-01T12:00:00Z
```

Reloading never activates a workflow. Activation requires both gates,
`--activate --yes`, and the exact previewed count. Historical backfill remains
off unless separately implemented and approved. Simulation calculates every
quiet-hours-adjusted step time but writes no database state and sends nothing.

## Segments and campaigns

Example product-buyer preview:

```bash
PYTHONPATH=src python3 -m seedhibaat segment preview \
  --kind product_buyers \
  --product-handle starter-product \
  --exclude-product-tag upgraded-product
```

Built-in kinds:

- `product_buyers`
- `not_reordered`
- `recent_purchasers`
- `lapsed_customers`
- `back_in_stock`

All automatically exclude suppressed customers, invalid numbers, and records
without a usable encrypted phone. Consent and product-conversion exclusions
remain explicit parts of each stored definition.

Campaign creation freezes a draft and sends nothing. Activation revalidates
the frozen audience and requires the reviewed recipient count.

## CSV sends

```bash
PYTHONPATH=src python3 -m seedhibaat send \
  --csv recipients.csv \
  --template approved_template \
  --language en_US \
  --body-param customer_name \
  --url-button-param 0:tracking_token
```

The command above is a dry run. A live send requires the identical reviewed
command plus `--send --yes`. The append-only ledger prevents identical repeats;
`--allow-resend` always requires separate approval.

CSV phone numbers should use E.164 format. For local-format imports, pass
`--default-country-code` or set `SEEDHIBAAT_DEFAULT_COUNTRY_CODE` in the
ignored environment.

## Metrics

```bash
PYTHONPATH=src python3 -m seedhibaat report
PYTHONPATH=src python3 -m seedhibaat report --workflow WORKFLOW_NAME
PYTHONPATH=src python3 -m seedhibaat report --campaign CAMPAIGN_ID
```

Observed-read is not an exact open rate because recipients can disable read
receipts. Signed redirects provide exact unique-click attribution for tracked
buttons.

### Read-only dashboard

Enable the optional aggregate dashboard with:

```dotenv
SEEDHIBAAT_METRICS_ENABLED=true
SEEDHIBAAT_METRICS_USERNAME=operator
SEEDHIBAAT_METRICS_PASSWORD=<independent-random-secret>
SEEDHIBAAT_REPORT_TIMEZONE=UTC
```

It is served at `/metrics` using HTTPS Basic authentication. It contains no
phone numbers, customer names, order details, rendered parameters, send
buttons, activation controls, or replay actions. The Python CLI remains the
only state-changing operator interface.

## Development

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
go test -race -timeout 90s ./...
go vet ./...
python3 tools/privacy_scan.py
git diff --check
go run ./cmd/seedhibaat-loadtest 10000
```

The load test does not contact Meta or Shopify.

## Deployment

The reference deployment uses a dedicated Linux user, loopback-only Go
service, Nginx, Let's Encrypt, systemd, SQLite WAL, and encrypted backups. It
does not install a separate queue or analytics service. Follow the
[production runbook](docs/runbook.md) and keep every real hostname, storefront
allowlist, provider identifier, and business workflow in ignored local/server
configuration.

## Private deployment configuration

Keep each real business deployment in an ignored `.sbconfig/` directory:

```text
.sbconfig/
  config.yaml
  workflows/
  templates/
```

Start from the tracked, placeholder-only example bundle:

```bash
cp -R .sbconfig.example .sbconfig
chmod -R go-rwx .sbconfig
git check-ignore .sbconfig/config.yaml
```

The example bundle documents the expected layout for both human operators and
coding agents. Never replace its placeholders with real deployment values;
make those edits only in the ignored `.sbconfig/` copy.

`config.yaml` may contain non-secret asset IDs, domains, template names, and
migration notes. Provider tokens, app secrets, signing keys, verification
codes, and PINs still belong only in `.env` or the owner-only server
environment. Deploy the private workflows with:

```bash
SEEDHIBAAT_WORKFLOW_SOURCE=.sbconfig/workflows tools/deploy.sh
```

## License

SeedhiBaat is licensed under the [MIT License](LICENSE).
