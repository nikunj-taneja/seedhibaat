package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type MetricsFilter struct {
	CampaignID   string
	WorkflowName string
	TemplateName string
}

type Metrics struct {
	From                string           `json:"from"`
	To                  string           `json:"to"`
	Attempted           int64            `json:"attempted"`
	Accepted            int64            `json:"accepted"`
	Sent                int64            `json:"sent"`
	Delivered           int64            `json:"delivered"`
	DeliveredRecipients int64            `json:"delivered_recipients"`
	ObservedRead        int64            `json:"observed_read"`
	Failed              int64            `json:"failed"`
	Replies             int64            `json:"replies"`
	Clicks              int64            `json:"clicks"`
	UniqueClicks        int64            `json:"unique_clicks"`
	OptOuts             int64            `json:"opt_outs"`
	Conversions         int64            `json:"conversions"`
	ConvertedRecipients int64            `json:"converted_recipients"`
	RevenueMinor        int64            `json:"revenue_minor"`
	RevenueByCurrency   []CurrencyAmount `json:"revenue_by_currency"`
	DeliveryRate        float64          `json:"delivery_rate"`
	ObservedReadRate    float64          `json:"observed_read_rate"`
	UniqueCTR           float64          `json:"unique_ctr"`
	ConversionRate      float64          `json:"conversion_rate"`
}

type CurrencyAmount struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

func (s *Store) Metrics(ctx context.Context, from, to time.Time) (Metrics, error) {
	return s.MetricsFiltered(ctx, from, to, MetricsFilter{})
}

func (s *Store) MetricsFiltered(ctx context.Context, from, to time.Time, filter MetricsFilter) (Metrics, error) {
	m := Metrics{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339)}
	messageConditions := []string{"m.created_at >= ?", "m.created_at < ?"}
	messageArgs := []any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	if filter.CampaignID != "" {
		messageConditions = append(messageConditions, "m.campaign_id=?")
		messageArgs = append(messageArgs, filter.CampaignID)
	}
	if filter.WorkflowName != "" {
		messageConditions = append(messageConditions, "EXISTS (SELECT 1 FROM workflow_runs wr WHERE wr.id=m.workflow_run_id AND wr.workflow_name=?)")
		messageArgs = append(messageArgs, filter.WorkflowName)
	}
	if filter.TemplateName != "" {
		messageConditions = append(messageConditions, "m.template_name=?")
		messageArgs = append(messageArgs, filter.TemplateName)
	}
	where := strings.Join(messageConditions, " AND ")
	query := `SELECT
		count(*) FILTER (WHERE attempted_at IS NOT NULL),
		count(*) FILTER (WHERE accepted_at IS NOT NULL),
		count(*) FILTER (WHERE sent_at IS NOT NULL),
		count(*) FILTER (WHERE delivered_at IS NOT NULL),
		count(*) FILTER (WHERE read_at IS NOT NULL),
		count(*) FILTER (WHERE failed_at IS NOT NULL),
		count(DISTINCT CASE WHEN delivered_at IS NOT NULL THEN customer_id END)
		FROM outbound_messages m WHERE ` + where
	if err := s.DB.QueryRowContext(ctx, query, messageArgs...).Scan(&m.Attempted, &m.Accepted, &m.Sent, &m.Delivered, &m.ObservedRead, &m.Failed, &m.DeliveredRecipients); err != nil {
		return Metrics{}, fmt.Errorf("message metrics: %w", err)
	}
	replyQuery := `SELECT count(*) FROM replies r WHERE r.received_at >= ? AND r.received_at < ?`
	replyArgs := []any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	if filter.CampaignID != "" || filter.WorkflowName != "" || filter.TemplateName != "" {
		replyConditions := append([]string{"r.received_at >= ?", "r.received_at < ?"}, messageConditions[2:]...)
		replyArgs = append(replyArgs, messageArgs[2:]...)
		replyQuery = `SELECT count(*) FROM replies r JOIN outbound_messages m ON m.meta_message_id=r.in_reply_to_meta_message_id WHERE ` + strings.Join(replyConditions, " AND ")
	}
	if err := s.DB.QueryRowContext(ctx, replyQuery, replyArgs...).Scan(&m.Replies); err != nil {
		return Metrics{}, err
	}
	clickConditions := append([]string{"l.created_at >= ?", "l.created_at < ?"}, messageConditions[2:]...)
	clickArgs := append([]any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}, messageArgs[2:]...)
	if err := s.DB.QueryRowContext(ctx, `SELECT coalesce(sum(l.click_count),0), count(DISTINCT CASE WHEN l.first_clicked_at IS NOT NULL THEN m.customer_id END) FROM tracked_links l JOIN outbound_messages m ON m.id=l.message_id WHERE `+strings.Join(clickConditions, " AND "), clickArgs...).Scan(&m.Clicks, &m.UniqueClicks); err != nil {
		return Metrics{}, err
	}
	optOutQuery := `SELECT count(*) FROM audit_log a WHERE a.action='customer.opt_out' AND a.occurred_at >= ? AND a.occurred_at < ?`
	optOutArgs := []any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	if filter.CampaignID != "" || filter.WorkflowName != "" || filter.TemplateName != "" {
		optOutQuery += ` AND EXISTS (SELECT 1 FROM outbound_messages m WHERE m.customer_id=CAST(a.object_id AS INTEGER) AND ` + where + `)`
		optOutArgs = append(optOutArgs, messageArgs...)
	}
	if err := s.DB.QueryRowContext(ctx, optOutQuery, optOutArgs...).Scan(&m.OptOuts); err != nil {
		return Metrics{}, err
	}
	conversionConditions := []string{"c.attributed_at >= ?", "c.attributed_at < ?"}
	conversionArgs := []any{from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	if filter.CampaignID != "" {
		conversionConditions = append(conversionConditions, "c.campaign_id=?")
		conversionArgs = append(conversionArgs, filter.CampaignID)
	}
	if filter.WorkflowName != "" {
		conversionConditions = append(conversionConditions, "EXISTS (SELECT 1 FROM workflow_runs wr WHERE wr.id=c.workflow_run_id AND wr.workflow_name=?)")
		conversionArgs = append(conversionArgs, filter.WorkflowName)
	}
	if filter.TemplateName != "" {
		conversionConditions = append(conversionConditions, "m.template_name=?")
		conversionArgs = append(conversionArgs, filter.TemplateName)
	}
	conversionFrom := ` FROM conversions c LEFT JOIN outbound_messages m ON m.id=c.message_id WHERE ` + strings.Join(conversionConditions, " AND ")
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT m.customer_id),coalesce(sum(c.amount_minor),0)`+conversionFrom, conversionArgs...).Scan(&m.Conversions, &m.ConvertedRecipients, &m.RevenueMinor); err != nil {
		return Metrics{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.currency,coalesce(sum(c.amount_minor),0)`+conversionFrom+` GROUP BY c.currency ORDER BY c.currency`, conversionArgs...)
	if err != nil {
		return Metrics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var amount CurrencyAmount
		if err := rows.Scan(&amount.Currency, &amount.AmountMinor); err != nil {
			return Metrics{}, err
		}
		m.RevenueByCurrency = append(m.RevenueByCurrency, amount)
	}
	if err := rows.Err(); err != nil {
		return Metrics{}, err
	}
	if m.Accepted > 0 {
		m.DeliveryRate = float64(m.Delivered) / float64(m.Accepted)
	}
	if m.Delivered > 0 {
		m.ObservedReadRate = float64(m.ObservedRead) / float64(m.Delivered)
	}
	if m.DeliveredRecipients > 0 {
		m.UniqueCTR = float64(m.UniqueClicks) / float64(m.DeliveredRecipients)
		m.ConversionRate = float64(m.ConvertedRecipients) / float64(m.DeliveredRecipients)
	}
	return m, nil
}
