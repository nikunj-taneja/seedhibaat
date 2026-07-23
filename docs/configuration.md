# Configuration reference

After copying `.env.example` to the ignored `.env`, generate the
application-owned secrets without displaying them:

```bash
PYTHONPATH=src python3 -m seedhibaat secrets init
```

The command uses cryptographic randomness, preserves existing provider values,
writes atomically, and sets mode `0600`.

`SEEDHIBAAT_REDIRECT_ALLOWED_HOSTS` is a comma-separated list of exact
storefront hostnames that signed tracked links may open. Do not include
schemes, paths, ports, or wildcard hosts. CSV and consent-import phone numbers
should use E.164; `SEEDHIBAAT_DEFAULT_COUNTRY_CODE` can prefix local-format
imports when an operator deliberately configures it.

## Workflow YAML

Definitions are immutable by `(name, version)`. Editing behavior requires a
version increment. Reloading validates and stores definitions but never
activates a workflow. `enabled` must remain `false`; activation exists only as
the count-confirmed operator command.

Required fields include:

- `trigger.type`: `order_delivered`, `inventory_back_in_stock`, or
  `manual_campaign`.
- `audience`: product handles/titles/tags and whether WhatsApp consent is
  required.
- `quiet_hours`: local start/end in `HH:MM` format.
- `frequency_cap`: maximum messages and a Go duration window.
- `conversion`: product handles/titles/tags that stop remaining steps.
- `steps`: unique ID, delay, approved template, language, explicit category,
  optional tracked destination, conditions and parameter mapping.

Parameter keys are contiguous one-based `header.N` or `body.N` positions.
Allowed sources are `customer.first_name`, `customer.last_name`,
`order.number`, `order.first_product_title`, and non-private `literal:` values.
Rendered values are never written to job payloads, logs or reports.

Step conditions can require the trigger order to remain uncancelled/unrefunded
and can require that a customer has or has not purchased products matching
configured handles, titles or tags. Every condition is re-evaluated immediately
before send.

All current automated customer messages must explicitly use `MARKETING`.
Waits support minutes, hours and days (`30m`, `6h`, `1d`, `7d`, or `1d12h`).

## Built-in segment kinds

- `product_buyers`
- `not_reordered`
- `recent_purchasers`
- `lapsed_customers`
- `back_in_stock`

Every segment automatically excludes suppressed customers, invalid numbers and
records without a callable encrypted phone. Consent and product-conversion
exclusions are explicit, recorded parts of the definition. Product-based
segments accept product handle/title selectors; conversion exclusions accept
`exclude_product_handle`, `exclude_product_title`, or `exclude_product_tag`.

## Metrics

Reports distinguish attempted, accepted, sent, delivered, observed-read,
failed, reply, total click, unique click, opt-out, conversion and attributed
revenue. Delivery rate, observed-read rate, exact unique CTR and conversion rate
are returned as decimal fractions. “Observed-read” is intentionally named:
WhatsApp read receipts depend
on recipient privacy settings and are not a complete open-rate measure.
Use `--campaign`, `--workflow`, or `--template` to filter a report. Set the
last-touch purchase window with `SEEDHIBAAT_ATTRIBUTION_WINDOW` (default
`720h`).

## Shopify reconciliation

The daemon reconciles Shopify once immediately at startup and then at
`SEEDHIBAAT_RECONCILE_INTERVAL`. On a new database it imports records updated
within `SEEDHIBAAT_INITIAL_SYNC_LOOKBACK` (default `17520h`, or 730 days).
After every complete sync it stores a durable watermark. Incremental runs begin
`SEEDHIBAAT_RECONCILE_OVERLAP` (default `24h`) before that watermark so late
and out-of-order updates are recovered without duplicating messages.
Pagination is paced and bounded at 50,000 records per resource per run, which
covers the default two-year order history at the expected store volume while
still preventing an unbounded synchronization loop.
