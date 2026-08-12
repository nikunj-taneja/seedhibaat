package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr                 string
	PublicBaseURL              string
	RedirectAllowedHosts       map[string]bool
	DatabasePath               string
	WorkflowDir                string
	LogLevel                   string
	APIKey                     string
	PIIHashKey                 string
	LinkSigningKey             string
	BackupKey                  string
	BackupDir                  string
	MetaAppSecret              string
	MetaVerifyToken            string
	MetaAccessToken            string
	MetaAPIVersion             string
	MetaWABAID                 string
	MetaPhoneNumberID          string
	MetaTestPhoneNumberID      string
	ShopifyShopDomain          string
	ShopifyClientID            string
	ShopifyClientSecret        string
	ShopifyAPIVersion          string
	GoKwikWebhookToken         string
	WorkerConcurrency          int
	WorkerPollInterval         time.Duration
	ReconcileInterval          time.Duration
	InitialSyncLookback        time.Duration
	ReconcileOverlap           time.Duration
	ShutdownTimeout            time.Duration
	ProductionFlowEnabled      bool
	OutboundSendingEnabled     bool
	RetentionDays              int
	AttributionWindow          time.Duration
	MetricsEnabled             bool
	MetricsUsername            string
	MetricsPassword            string
	ReportTimezone             string
	MarketingMessageCostMicros int64
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:                 env("SEEDHIBAAT_LISTEN_ADDR", "127.0.0.1:8088"),
		PublicBaseURL:              strings.TrimRight(os.Getenv("SEEDHIBAAT_PUBLIC_BASE_URL"), "/"),
		RedirectAllowedHosts:       envHosts("SEEDHIBAAT_REDIRECT_ALLOWED_HOSTS"),
		DatabasePath:               env("SEEDHIBAAT_DATABASE_PATH", "state/seedhibaat.db"),
		WorkflowDir:                env("SEEDHIBAAT_WORKFLOW_DIR", "config/workflows"),
		LogLevel:                   env("SEEDHIBAAT_LOG_LEVEL", "info"),
		APIKey:                     os.Getenv("SEEDHIBAAT_API_KEY"),
		PIIHashKey:                 os.Getenv("SEEDHIBAAT_PII_HASH_KEY"),
		LinkSigningKey:             os.Getenv("SEEDHIBAAT_LINK_SIGNING_KEY"),
		BackupKey:                  os.Getenv("SEEDHIBAAT_BACKUP_KEY"),
		BackupDir:                  env("SEEDHIBAAT_BACKUP_DIR", "backups"),
		MetaAppSecret:              os.Getenv("META_APP_SECRET"),
		MetaVerifyToken:            os.Getenv("META_WEBHOOK_VERIFY_TOKEN"),
		MetaAccessToken:            os.Getenv("META_SYSTEM_USER_TOKEN"),
		MetaAPIVersion:             env("META_GRAPH_API_VERSION", "v23.0"),
		MetaWABAID:                 os.Getenv("WHATSAPP_BUSINESS_ACCOUNT_ID"),
		MetaPhoneNumberID:          os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		MetaTestPhoneNumberID:      os.Getenv("WHATSAPP_TEST_PHONE_NUMBER_ID"),
		ShopifyShopDomain:          os.Getenv("SHOPIFY_SHOP_DOMAIN"),
		ShopifyClientID:            os.Getenv("SHOPIFY_CLIENT_ID"),
		ShopifyClientSecret:        os.Getenv("SHOPIFY_CLIENT_SECRET"),
		ShopifyAPIVersion:          env("SHOPIFY_API_VERSION", "2026-07"),
		GoKwikWebhookToken:         os.Getenv("GOKWIK_WEBHOOK_TOKEN"),
		WorkerConcurrency:          envInt("SEEDHIBAAT_WORKER_CONCURRENCY", 4),
		WorkerPollInterval:         envDuration("SEEDHIBAAT_WORKER_POLL_INTERVAL", time.Second),
		ReconcileInterval:          envDuration("SEEDHIBAAT_RECONCILE_INTERVAL", 6*time.Hour),
		InitialSyncLookback:        envDuration("SEEDHIBAAT_INITIAL_SYNC_LOOKBACK", 2*365*24*time.Hour),
		ReconcileOverlap:           envDuration("SEEDHIBAAT_RECONCILE_OVERLAP", 24*time.Hour),
		ShutdownTimeout:            envDuration("SEEDHIBAAT_SHUTDOWN_TIMEOUT", 20*time.Second),
		ProductionFlowEnabled:      envBool("SEEDHIBAAT_PRODUCTION_FLOW_ENABLED", false),
		OutboundSendingEnabled:     envBool("SEEDHIBAAT_OUTBOUND_SENDING_ENABLED", false),
		RetentionDays:              envInt("SEEDHIBAAT_RETENTION_DAYS", 365),
		AttributionWindow:          envDuration("SEEDHIBAAT_ATTRIBUTION_WINDOW", 30*24*time.Hour),
		MetricsEnabled:             envBool("SEEDHIBAAT_METRICS_ENABLED", false),
		MetricsUsername:            env("SEEDHIBAAT_METRICS_USERNAME", "operator"),
		MetricsPassword:            os.Getenv("SEEDHIBAAT_METRICS_PASSWORD"),
		ReportTimezone:             env("SEEDHIBAAT_REPORT_TIMEZONE", "UTC"),
		MarketingMessageCostMicros: int64(envInt("SEEDHIBAAT_MARKETING_MESSAGE_COST_MICROS", 0)),
	}
	if c.WorkerConcurrency < 1 || c.WorkerConcurrency > 32 {
		return Config{}, errors.New("SEEDHIBAAT_WORKER_CONCURRENCY must be between 1 and 32")
	}
	if c.RetentionDays < 30 {
		return Config{}, errors.New("SEEDHIBAAT_RETENTION_DAYS must be at least 30")
	}
	if c.MarketingMessageCostMicros < 0 {
		return Config{}, errors.New("SEEDHIBAAT_MARKETING_MESSAGE_COST_MICROS must be non-negative")
	}
	if c.GoKwikWebhookToken != "" && len(c.GoKwikWebhookToken) < 32 {
		return Config{}, errors.New("GOKWIK_WEBHOOK_TOKEN must contain at least 32 characters")
	}
	if c.AttributionWindow <= 0 || c.AttributionWindow > 365*24*time.Hour {
		return Config{}, errors.New("SEEDHIBAAT_ATTRIBUTION_WINDOW must be between 1ns and 8760h")
	}
	if c.InitialSyncLookback <= 0 || c.InitialSyncLookback > 10*365*24*time.Hour {
		return Config{}, errors.New("SEEDHIBAAT_INITIAL_SYNC_LOOKBACK must be between 1ns and 87600h")
	}
	if c.ReconcileOverlap < 0 || c.ReconcileOverlap > 30*24*time.Hour {
		return Config{}, errors.New("SEEDHIBAAT_RECONCILE_OVERLAP must be between 0 and 720h")
	}
	if _, err := time.LoadLocation(c.ReportTimezone); err != nil {
		return Config{}, fmt.Errorf("SEEDHIBAAT_REPORT_TIMEZONE is invalid: %w", err)
	}
	return c, nil
}

func (c Config) ValidateForServe() error {
	var missing []string
	for name, value := range map[string]string{
		"SEEDHIBAAT_API_KEY":           c.APIKey,
		"SEEDHIBAAT_PII_HASH_KEY":      c.PIIHashKey,
		"SEEDHIBAAT_LINK_SIGNING_KEY":  c.LinkSigningKey,
		"SEEDHIBAAT_BACKUP_KEY":        c.BackupKey,
		"META_APP_SECRET":              c.MetaAppSecret,
		"META_WEBHOOK_VERIFY_TOKEN":    c.MetaVerifyToken,
		"META_SYSTEM_USER_TOKEN":       c.MetaAccessToken,
		"WHATSAPP_BUSINESS_ACCOUNT_ID": c.MetaWABAID,
		"WHATSAPP_PHONE_NUMBER_ID":     c.MetaPhoneNumberID,
		"SHOPIFY_SHOP_DOMAIN":          c.ShopifyShopDomain,
		"SHOPIFY_CLIENT_ID":            c.ShopifyClientID,
		"SHOPIFY_CLIENT_SECRET":        c.ShopifyClientSecret,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required secrets: %s", strings.Join(missing, ", "))
	}
	for name, value := range map[string]string{"SEEDHIBAAT_API_KEY": c.APIKey, "SEEDHIBAAT_PII_HASH_KEY": c.PIIHashKey, "SEEDHIBAAT_LINK_SIGNING_KEY": c.LinkSigningKey, "SEEDHIBAAT_BACKUP_KEY": c.BackupKey, "META_WEBHOOK_VERIFY_TOKEN": c.MetaVerifyToken} {
		if len(value) < 32 {
			return fmt.Errorf("%s must contain at least 32 characters", name)
		}
	}
	publicURL, err := url.Parse(c.PublicBaseURL)
	if err != nil || publicURL.Scheme != "https" || publicURL.Hostname() == "" {
		return errors.New("SEEDHIBAAT_PUBLIC_BASE_URL must be an absolute HTTPS URL")
	}
	if len(c.RedirectAllowedHosts) == 0 {
		return errors.New("SEEDHIBAAT_REDIRECT_ALLOWED_HOSTS must contain at least one hostname")
	}
	if c.MetricsEnabled {
		if strings.TrimSpace(c.MetricsUsername) == "" {
			return errors.New("SEEDHIBAAT_METRICS_USERNAME is required when metrics are enabled")
		}
		if len(c.MetricsPassword) < 32 {
			return errors.New("SEEDHIBAAT_METRICS_PASSWORD must contain at least 32 characters when metrics are enabled")
		}
	}
	return nil
}

func envHosts(name string) map[string]bool {
	result := make(map[string]bool)
	for _, value := range strings.Split(os.Getenv(name), ",") {
		host := strings.ToLower(strings.TrimSpace(value))
		if host == "" {
			continue
		}
		if strings.ContainsAny(host, "/:@") || strings.Contains(host, " ") {
			continue
		}
		result[host] = true
	}
	return result
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
