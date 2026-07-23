# Production runbook

`AGENTS.md` is authoritative for safety and approval rules. The procedures
below apply those rules; they do not override them.

## Operator procedures

### CSV sends

1. Obtain the CSV path, approved template name and language, and ordered
   header/body/URL-button mappings.
2. Inspect only the header and row count, then run `seedhibaat send` without
   `--send`.
3. Report the exact template, validated message count, and unique valid
   recipient count. Stop on any validation error.
4. Require explicit approval for that exact template and both counts. Only
   then run the identical command with `--send --yes`.
5. Report accepted, failed, and duplicate-skipped counts. API acceptance is
   not evidence of delivery.

The owner-designated test number is supplied out of band and must never enter
tracked files or fixtures. Every repeat requires separate approval and
`--allow-resend` must never be inferred.

### Templates

1. Validate with `seedhibaat template submit --file PATH`.
2. Show the name, language, category, body, button text, and URL shape.
3. Require the operator to explicitly approve that exact displayed definition.
   General authorization to build or deploy SeedhiBaat is not template
   approval.
4. Submission mutates Meta. Use `--submit --yes` only after the exact approval.
5. Poll with `seedhibaat template status NAME`; send only after `APPROVED`.

### Segments and campaigns

1. Preview with `seedhibaat segment preview`; report only aggregate counts and
   the exact definition and exclusions.
2. `campaign create` creates a frozen draft and sends nothing.
3. Before activation, show the campaign ID, template, category, schedule,
   tracked destination, frequency cap, segment, exclusions, and frozen count.
4. Require explicit approval for that count. Activation requires both
   production gates and `campaign activate --confirmed-count N --activate
   --yes`.

### Always-on workflows

1. Validate YAML with `seedhibaat workflow validate --file PATH`; reloading a
   definition does not activate it.
2. Before activation, show every step, delay, template, category, quiet hours,
   frequency cap, conversion rule, suppression rule, and current audience
   count.
3. Require explicit approval for that count. Activation requires both
   production gates and `--confirmed-count N --activate --yes`. Historical
   backfill remains off unless separately approved.

### Recovery

1. Inspect aggregate reports and `seedhibaat audit` without printing payloads
   or customer PII.
2. Pause or cancel the affected run or campaign before changing queue state.
3. Manual replay requires `--replay --yes` and is limited to failed jobs.
   Reconcile accepted, delivered, read, and unknown outcomes instead of
   replaying them.

## Prerequisites

- The configured public hostname, such as `wa.example.com`, points to the VPS.
- The Shopify app is installed with the documented scopes and protected
  customer-data access.
- Meta and Shopify credentials are available locally and have never been
  committed.
- Meta's app secret and a permanent system-user token are available.
- The active WABA and phone-number ID pair has been verified by
  `seedhibaat meta identity`; the preserved fallback pair passes
  `seedhibaat meta identity --profile test`.

## Build and stage

The VPS is x86-64 and does not need a Go toolchain. Build a static binary on the
operator machine:

```bash
GO_BIN=/path/to/go tools/deploy.sh
```

The script creates a dedicated `seedhibaat` system user, installs files under
`/opt/seedhibaat`, writes only SeedhiBaat systemd units, and leaves the service
stopped. It does not modify unrelated services. Set
`SEEDHIBAAT_WORKFLOW_SOURCE=.sbconfig/workflows` to deploy private,
business-specific definitions rather than the tracked disabled examples.

## Provision secrets

Copy `deploy/seedhibaat.env.example` to
`/etc/seedhibaat/seedhibaat.env` on the VPS. Fill it directly on the server and
set ownership/mode:

```bash
chown root:root /etc/seedhibaat/seedhibaat.env
chmod 600 /etc/seedhibaat/seedhibaat.env
```

Generate independent random values for the API, PII, link, backup and webhook
verification keys. Do not reuse a provider secret. Leave both production gates
false.

The local operator can fill only empty internal-key entries without printing
or replacing existing values, then securely provision the resulting ignored
environment file:

```bash
PYTHONPATH=src python3 -m seedhibaat secrets init --env-file .env
```

## Nginx and TLS

After DNS resolves to the VPS:

```bash
install -o root -g root -m 0644 deploy/nginx-seedhibaat.conf \
  /etc/nginx/sites-available/wa.example.com
ln -s /etc/nginx/sites-available/wa.example.com \
  /etc/nginx/sites-enabled/wa.example.com
nginx -t
systemctl reload nginx
certbot --nginx -d wa.example.com
nginx -t
```

The public virtual host exposes health, two webhook endpoints, tracked
redirects, and—when explicitly enabled—the independently authenticated
read-only metrics dashboard. The state-changing operator API remains
accessible only on the VPS loopback interface.

Generate a separate dashboard password with `seedhibaat secrets init`, set
`SEEDHIBAAT_METRICS_ENABLED=true`, and expose only `/metrics` plus its static
asset path through Nginx. Do not reuse the operator API key.

## First start

```bash
systemctl start seedhibaat
systemctl enable --now seedhibaat-backup.timer
systemctl status seedhibaat --no-pager
curl --fail http://127.0.0.1:8088/healthz
journalctl -u seedhibaat -n 100 --no-pager
```

Verify that both gates are reported false. Run `seedhibaatd integrity` and a
manual `seedhibaat-backup.service` before configuring providers.

## Provider webhooks

Configure Meta's callback as `https://wa.example.com/webhooks/meta` using
the secret verification token in the server environment. Subscribe the WABA
to message events. Configure Shopify webhook topics for orders, fulfillments,
refunds, returns, customers, products and inventory at
`https://wa.example.com/webhooks/shopify`.
In the Shopify app configuration or Dev Dashboard, separately point the
mandatory `customers/data_request`, `customers/redact`, and `shop/redact`
privacy topics at that endpoint. The normal Admin GraphQL registration command
cannot create these compliance subscriptions.

Send signed webhook fixtures first. A bad signature must return 401; a repeated
Shopify webhook ID must return success with `duplicate: true`.

## Backups and restore drill

The daily timer creates an AES-256-GCM encrypted SQLite snapshot in
`/var/backups/seedhibaat` and prunes files older than the configured retention
period. The encryption key must also be stored in an offline password manager;
backups are unrecoverable without it.

Restore drills must target a new path while the live service remains untouched.
After restoration, run an integrity check and compare aggregate table counts.
Never overwrite the live database during a drill.

```bash
set -a
source /etc/seedhibaat/seedhibaat.env
set +a
/opt/seedhibaat/bin/seedhibaatd restore \
  /var/backups/seedhibaat/seedhibaat-TIMESTAMP.db.enc \
  /var/lib/seedhibaat/restore-drill.db
SEEDHIBAAT_DATABASE_PATH=/var/lib/seedhibaat/restore-drill.db \
  /opt/seedhibaat/bin/seedhibaatd integrity
```

Supply the backup key through the owner-only environment in real operation;
do not place it literally in shell history.

## Incident actions

- Unexpected sends: set both gates false and restart `seedhibaat`.
- Meta credential exposure: revoke the token, rotate the app secret if needed,
  update the environment file, and restart.
- Shopify credential exposure: rotate the client secret, update the environment
  file, and restart.
- Queue anomaly: pause the workflow/campaign; do not delete the database or
  ledger. Inspect aggregate state and audit history first.
- Suspected duplicate: do not use `--allow-resend`; reconcile Meta message IDs
  and webhook events before any manual replay.
- A failed job can be replayed with `seedhibaat job replay ID --replay --yes`.
  Unknown or possibly accepted provider states are deliberately not replayable.
