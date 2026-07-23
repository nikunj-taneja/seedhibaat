package store

import (
	"context"
	"database/sql"
	"time"
)

type DailyMetric struct {
	Date                string
	Accepted            int64
	Delivered           int64
	ObservedRead        int64
	UniqueClicks        int64
	ConvertedRecipients int64
}

type PerformanceRow struct {
	Kind                string
	Name                string
	TemplateName        string
	Accepted            int64
	Delivered           int64
	DeliveredRecipients int64
	ObservedRead        int64
	Failed              int64
	Clicks              int64
	UniqueClicks        int64
	ConvertedRecipients int64
	Conversions         int64
	RevenueMinor        int64
	Currencies          string
}

func (s *Store) EarliestMessageTime(ctx context.Context) (time.Time, bool, error) {
	var value sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT min(created_at) FROM outbound_messages`).Scan(&value); err != nil {
		return time.Time{}, false, err
	}
	if !value.Valid {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

func (s *Store) DailyMetrics(ctx context.Context, from, to time.Time, location *time.Location) ([]DailyMetric, error) {
	localFrom := from.In(location)
	cursor := time.Date(localFrom.Year(), localFrom.Month(), localFrom.Day(), 0, 0, 0, 0, location)
	var result []DailyMetric
	for cursor.Before(to) && len(result) < 120 {
		next := cursor.AddDate(0, 0, 1)
		dayTo := next
		if dayTo.After(to) {
			dayTo = to
		}
		dayFrom := cursor
		if dayFrom.Before(from) {
			dayFrom = from
		}
		metrics, err := s.Metrics(ctx, dayFrom, dayTo)
		if err != nil {
			return nil, err
		}
		result = append(result, DailyMetric{
			Date:                cursor.Format("Jan 2"),
			Accepted:            metrics.Accepted,
			Delivered:           metrics.Delivered,
			ObservedRead:        metrics.ObservedRead,
			UniqueClicks:        metrics.UniqueClicks,
			ConvertedRecipients: metrics.ConvertedRecipients,
		})
		cursor = next
	}
	return result, nil
}

func (s *Store) PerformanceBreakdown(ctx context.Context, from, to time.Time) ([]PerformanceRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH base AS (
			SELECT
				m.*,
				CASE
					WHEN m.campaign_id IS NOT NULL THEN 'Campaign'
					WHEN m.workflow_run_id IS NOT NULL THEN 'Workflow'
					ELSE 'Direct'
				END AS source_kind,
				CASE
					WHEN m.campaign_id IS NOT NULL THEN coalesce(cp.name,m.campaign_id)
					WHEN m.workflow_run_id IS NOT NULL THEN coalesce(wr.workflow_name,'Workflow')
					ELSE 'CSV / direct send'
				END AS source_name
			FROM outbound_messages m
			LEFT JOIN campaigns cp ON cp.id=m.campaign_id
			LEFT JOIN workflow_runs wr ON wr.id=m.workflow_run_id
			WHERE m.created_at >= ? AND m.created_at < ?
		),
		link_totals AS (
			SELECT message_id,sum(click_count) AS clicks,max(CASE WHEN first_clicked_at IS NOT NULL THEN 1 ELSE 0 END) AS clicked
			FROM tracked_links GROUP BY message_id
		),
		conversion_totals AS (
			SELECT message_id,count(*) AS conversions,sum(amount_minor) AS revenue_minor,group_concat(DISTINCT currency) AS currencies
			FROM conversions GROUP BY message_id
		)
		SELECT
			b.source_kind,
			b.source_name,
			b.template_name,
			count(*) FILTER (WHERE b.accepted_at IS NOT NULL),
			count(*) FILTER (WHERE b.delivered_at IS NOT NULL),
			count(DISTINCT CASE WHEN b.delivered_at IS NOT NULL THEN b.customer_id END),
			count(*) FILTER (WHERE b.read_at IS NOT NULL),
			count(*) FILTER (WHERE b.failed_at IS NOT NULL),
			coalesce(sum(lt.clicks),0),
			count(DISTINCT CASE WHEN lt.clicked=1 THEN b.customer_id END),
			count(DISTINCT CASE WHEN ct.conversions > 0 THEN b.customer_id END),
			coalesce(sum(ct.conversions),0),
			coalesce(sum(ct.revenue_minor),0),
			coalesce(group_concat(DISTINCT ct.currencies),'')
		FROM base b
		LEFT JOIN link_totals lt ON lt.message_id=b.id
		LEFT JOIN conversion_totals ct ON ct.message_id=b.id
		GROUP BY b.source_kind,b.source_name,b.template_name
		ORDER BY count(*) FILTER (WHERE b.delivered_at IS NOT NULL) DESC,b.source_name,b.template_name
		LIMIT 50`,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PerformanceRow
	for rows.Next() {
		var row PerformanceRow
		if err := rows.Scan(
			&row.Kind,
			&row.Name,
			&row.TemplateName,
			&row.Accepted,
			&row.Delivered,
			&row.DeliveredRecipients,
			&row.ObservedRead,
			&row.Failed,
			&row.Clicks,
			&row.UniqueClicks,
			&row.ConvertedRecipients,
			&row.Conversions,
			&row.RevenueMinor,
			&row.Currencies,
		); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
