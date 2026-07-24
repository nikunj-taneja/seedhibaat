# Architecture

SeedhiBaat deliberately uses one small Go process, one SQLite database, and the
existing Nginx host. Python remains the state-changing human-facing CLI. The
only browser surface is an optional authenticated, read-only aggregate metrics
dashboard; there is no separate queue or analytics service.

```text
Shopify webhooks ─┐
                  ├─> Nginx ─> signature check ─> durable webhook inbox
Meta webhooks ────┘                                  │
                                                     v
Shopify GraphQL <─ reconciliation worker <─ SQLite ─> workflow scheduler
                                                     │
                                                     v
Meta Cloud API <──────── bounded outbound workers <─ queue
                                                     │
Customer click ─> signed /r/ link ─> click event ────┘

Python CLI ─> authenticated loopback JSON API ─> previews, reports and controls
Browser ─> Basic Auth ─> aggregate read-only /metrics dashboard
```

## Failure boundaries

- Webhook handlers verify the raw body, insert an idempotent inbox row, and
  respond quickly. Processing is asynchronous.
- Shopify's `X-Shopify-Webhook-Id` is the deduplication key. Exact duplicate
  Meta bodies are deduplicated at ingress; individual Meta status events also
  have a unique event fingerprint.
- Each workflow run is unique for workflow version, customer, trigger type,
  and trigger ID. Each step has its own unique idempotency key.
- Queue claims and leases are transactional. Stale leases are recovered after
  a restart. Retry delay is bounded exponential backoff.
- An outbound message stores Meta's message ID and its accepted, sent,
  delivered, read and failed timestamps independently, so out-of-order events
  do not destroy information.
- A signed Meta status callback is applied only when its message ID is already
  known locally. This lets delayed test-profile receipts arrive after the
  active sending profile returns to production. Unsolicited inbound messages
  remain restricted to the active and configured test phone-number IDs.
- API acceptance sets only `accepted_at`. Delivery is set only by a verified
  `delivered` webhook.

## Delivery and suppression model

An order is fully delivered only when every line item's current quantity is
covered by a fulfillment with a non-null `deliveredAt`. This avoids starting a
flow on fulfillment creation or a partial shipment.

Every job rechecks eligibility immediately before sending. A job is suppressed
when consent is absent, the customer is suppressed or invalid, its workflow is
paused/cancelled, a conversion has completed, or the frequency cap is reached.
Workflow jobs also recheck their timezone and quiet window immediately before
sending, so retries and resumed jobs are durably deferred to the next allowed
time.
Refunds, cancellations, active returns, STOP replies and configured conversion
purchases cancel remaining work transactionally.
Refund and return state is persisted and excluded from every purchase-derived
segment. Campaign activation re-runs the stored segment and rejects the draft
if even one customer changed since preview, forcing a new reviewed count.

Step conditions are stored with each durable job and re-evaluated just before
send. Template bindings store only allowlisted source names. Names/order values
are decrypted or loaded in memory at send time; only a keyed fingerprint of the
rendered parameter sequence is retained.

## Privacy model

- Phone numbers and names are AES-GCM encrypted at rest.
- Deterministic customer lookup uses an HMAC phone hash, not plaintext.
- Raw webhook bodies are retained only until successful processing and then
  replaced by their digest.
- Reports and previews contain aggregate counts only.
- The optional dashboard uses independent credentials, contains aggregate
  results only, and exposes no state-changing actions.
- Redirect tokens are signed, expiring and mapped to a server-side HTTPS
  destination allowlist. They contain no customer identifier.
- Nginx access logging is disabled for Meta verification and tracked redirects
  so verification and per-recipient tokens never enter proxy logs.
- Secrets are provided only through an owner-only environment file on the VPS.

## Safety gates

`SEEDHIBAAT_OUTBOUND_SENDING_ENABLED` controls whether any daemon worker may
contact the Meta send endpoint. `SEEDHIBAAT_PRODUCTION_FLOW_ENABLED` controls
campaign/workflow activation. Both default to false, and campaign activation
also requires the frozen audience count to be repeated exactly.
