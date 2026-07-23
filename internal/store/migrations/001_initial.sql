PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS webhook_events (
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    received_at TEXT NOT NULL,
    occurred_at TEXT,
    available_at TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    payload BLOB NOT NULL,
    error TEXT,
    PRIMARY KEY (provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_webhook_pending ON webhook_events(status, received_at);

CREATE TABLE IF NOT EXISTS customers (
    id INTEGER PRIMARY KEY,
    shopify_id TEXT UNIQUE,
    phone_ciphertext BLOB,
    phone_hash TEXT UNIQUE,
    first_name_ciphertext BLOB,
    last_name_ciphertext BLOB,
    timezone TEXT NOT NULL DEFAULT 'Asia/Kolkata',
    whatsapp_consent TEXT NOT NULL DEFAULT 'unknown',
    consent_updated_at TEXT,
    invalid_number INTEGER NOT NULL DEFAULT 0,
    suppressed_at TEXT,
    suppression_reason TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_customers_consent ON customers(whatsapp_consent, suppressed_at);

CREATE TABLE IF NOT EXISTS products (
    shopify_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    handle TEXT,
    product_type TEXT,
    status TEXT,
    tags_json TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS variants (
    shopify_id TEXT PRIMARY KEY,
    product_id TEXT NOT NULL REFERENCES products(shopify_id) ON DELETE CASCADE,
    title TEXT,
    sku TEXT,
    inventory_item_id TEXT,
    inventory_quantity INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_variants_product ON variants(product_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_variants_inventory_item ON variants(inventory_item_id) WHERE inventory_item_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS inventory_levels (
    inventory_item_id TEXT NOT NULL,
    location_id TEXT NOT NULL,
    variant_id TEXT,
    available INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(inventory_item_id, location_id)
);
CREATE INDEX IF NOT EXISTS idx_inventory_available ON inventory_levels(variant_id, available);

CREATE TABLE IF NOT EXISTS orders (
    shopify_id TEXT PRIMARY KEY,
    customer_id INTEGER REFERENCES customers(id),
    order_number TEXT,
    processed_at TEXT,
    cancelled_at TEXT,
    financial_status TEXT,
    currency TEXT,
    total_amount_minor INTEGER,
    fully_delivered_at TEXT,
    refunded_at TEXT,
    raw_updated_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id, processed_at);
CREATE INDEX IF NOT EXISTS idx_orders_delivery ON orders(fully_delivered_at);

CREATE TABLE IF NOT EXISTS order_lines (
    shopify_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(shopify_id) ON DELETE CASCADE,
    product_id TEXT,
    variant_id TEXT,
    title TEXT NOT NULL,
    sku TEXT,
    quantity INTEGER NOT NULL,
    current_quantity INTEGER NOT NULL,
    delivered_quantity INTEGER NOT NULL DEFAULT 0,
    refunded_quantity INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_order_lines_order ON order_lines(order_id);
CREATE INDEX IF NOT EXISTS idx_order_lines_product ON order_lines(product_id, variant_id);

CREATE TABLE IF NOT EXISTS fulfillments (
    shopify_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(shopify_id) ON DELETE CASCADE,
    status TEXT,
    delivered_at TEXT,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fulfillments_order ON fulfillments(order_id);

CREATE TABLE IF NOT EXISTS workflow_definitions (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    definition_hash TEXT NOT NULL,
    yaml TEXT NOT NULL,
    active INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY(name, version)
);

CREATE TABLE IF NOT EXISTS workflow_runs (
    id TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    workflow_version INTEGER NOT NULL,
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    trigger_type TEXT NOT NULL,
    trigger_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'active',
    started_at TEXT NOT NULL,
    completed_at TEXT,
    cancelled_at TEXT,
    cancellation_reason TEXT,
    UNIQUE(workflow_name, workflow_version, customer_id, trigger_type, trigger_id)
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_customer ON workflow_runs(customer_id, state);

CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id TEXT PRIMARY KEY,
    workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    payload BLOB NOT NULL,
    state TEXT NOT NULL DEFAULT 'scheduled',
    scheduled_at TEXT NOT NULL,
    available_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 8,
    locked_at TEXT,
    locked_by TEXT,
    last_error TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON scheduled_jobs(state, available_at, scheduled_at);
CREATE INDEX IF NOT EXISTS idx_jobs_run ON scheduled_jobs(workflow_run_id, state);

CREATE TABLE IF NOT EXISTS outbound_messages (
    id TEXT PRIMARY KEY,
    job_id TEXT UNIQUE REFERENCES scheduled_jobs(id),
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    workflow_run_id TEXT REFERENCES workflow_runs(id),
    campaign_id TEXT,
    template_name TEXT NOT NULL,
    template_language TEXT NOT NULL,
    category TEXT NOT NULL,
    parameter_fingerprint TEXT,
    idempotency_key TEXT NOT NULL UNIQUE,
    meta_message_id TEXT UNIQUE,
    state TEXT NOT NULL DEFAULT 'queued',
    attempted_at TEXT,
    accepted_at TEXT,
    sent_at TEXT,
    delivered_at TEXT,
    read_at TEXT,
    failed_at TEXT,
    failure_code TEXT,
    failure_reason TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_reporting ON outbound_messages(created_at, state, template_name);
CREATE INDEX IF NOT EXISTS idx_messages_customer ON outbound_messages(customer_id, created_at);

CREATE TABLE IF NOT EXISTS message_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id TEXT NOT NULL REFERENCES outbound_messages(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    provider_timestamp TEXT,
    received_at TEXT NOT NULL,
    payload_fingerprint TEXT NOT NULL,
    UNIQUE(message_id, event_type, provider_timestamp, payload_fingerprint)
);

CREATE TABLE IF NOT EXISTS campaigns (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    segment_json TEXT NOT NULL,
    exclusions_json TEXT NOT NULL,
    template_name TEXT NOT NULL,
    template_language TEXT NOT NULL,
    scheduled_at TEXT,
    state TEXT NOT NULL DEFAULT 'draft',
    audience_count INTEGER,
    created_at TEXT NOT NULL,
    activated_at TEXT
);

CREATE TABLE IF NOT EXISTS campaign_recipients (
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    exclusion_reason TEXT,
    queued_at TEXT,
    PRIMARY KEY(campaign_id, customer_id)
);

CREATE TABLE IF NOT EXISTS tracked_links (
    token_hash TEXT PRIMARY KEY,
    message_id TEXT NOT NULL REFERENCES outbound_messages(id) ON DELETE CASCADE,
    destination_url TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    first_clicked_at TEXT,
    click_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id TEXT NOT NULL REFERENCES orders(shopify_id),
    message_id TEXT REFERENCES outbound_messages(id),
    campaign_id TEXT,
    workflow_run_id TEXT,
    attributed_at TEXT NOT NULL,
    amount_minor INTEGER NOT NULL,
    currency TEXT NOT NULL,
    attribution_model TEXT NOT NULL,
    UNIQUE(order_id, message_id, attribution_model)
);

CREATE TABLE IF NOT EXISTS replies (
    provider_message_id TEXT PRIMARY KEY,
    customer_id INTEGER REFERENCES customers(id),
    in_reply_to_meta_message_id TEXT,
    received_at TEXT NOT NULL,
    message_type TEXT NOT NULL,
    body_ciphertext BLOB,
    body_hash TEXT
);

CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at TEXT NOT NULL,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT,
    details_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(occurred_at);

CREATE TABLE IF NOT EXISTS sync_cursors (
    source TEXT NOT NULL,
    resource TEXT NOT NULL,
    cursor TEXT,
    watermark TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(source, resource)
);

CREATE TABLE IF NOT EXISTS frequency_caps (
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    window_start TEXT NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(customer_id, window_start)
);
