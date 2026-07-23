package segment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Definition struct {
	Kind                 string `json:"kind"`
	ProductHandle        string `json:"product_handle,omitempty"`
	ProductTitle         string `json:"product_title,omitempty"`
	WithinDays           int    `json:"within_days,omitempty"`
	LapsedDays           int    `json:"lapsed_days,omitempty"`
	RequireConsent       bool   `json:"require_whatsapp_consent"`
	ExcludeProductHandle string `json:"exclude_product_handle,omitempty"`
	ExcludeProductTitle  string `json:"exclude_product_title,omitempty"`
	ExcludeProductTag    string `json:"exclude_product_tag,omitempty"`
	ExcludeRecentDays    int    `json:"exclude_recent_purchase_days,omitempty"`
}

type Result struct {
	CustomerIDs   []int64          `json:"-"`
	EligibleCount int              `json:"eligible_count"`
	Definition    Definition       `json:"definition"`
	Exclusions    map[string]int64 `json:"exclusions"`
}

func Preview(ctx context.Context, db *sql.DB, definition Definition, now time.Time) (Result, error) {
	if err := definition.Validate(); err != nil {
		return Result{}, err
	}
	where := []string{"c.suppressed_at IS NULL", "c.invalid_number=0", "c.phone_ciphertext IS NOT NULL"}
	args := []any{}
	if definition.RequireConsent {
		where = append(where, "c.whatsapp_consent='opted_in'")
	}
	purchaseProductCondition, purchaseProductArgs := productCondition(
		"p", "ol", definition.ProductHandle, definition.ProductTitle, "",
	)
	catalogProductCondition, catalogProductArgs := productCondition(
		"p", "", definition.ProductHandle, definition.ProductTitle, "",
	)
	switch definition.Kind {
	case "product_buyers":
		where = append(where, `EXISTS (SELECT 1 FROM orders o JOIN order_lines ol ON ol.order_id=o.shopify_id LEFT JOIN products p ON p.shopify_id=ol.product_id WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND `+purchaseProductCondition+`)`)
		args = append(args, purchaseProductArgs...)
	case "not_reordered":
		where = append(where, `(SELECT count(*) FROM orders o WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL)=1`)
	case "recent_purchasers":
		where = append(where, `EXISTS (SELECT 1 FROM orders o WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND o.processed_at>=?)`)
		args = append(args, now.AddDate(0, 0, -definition.WithinDays).UTC().Format(time.RFC3339))
	case "lapsed_customers":
		where = append(where, `EXISTS (SELECT 1 FROM orders o WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL)`, `NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND o.processed_at>=?)`)
		args = append(args, now.AddDate(0, 0, -definition.LapsedDays).UTC().Format(time.RFC3339))
	case "back_in_stock":
		where = append(where,
			`EXISTS (SELECT 1 FROM orders o JOIN order_lines ol ON ol.order_id=o.shopify_id LEFT JOIN products p ON p.shopify_id=ol.product_id WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND `+purchaseProductCondition+`)`,
			`EXISTS (SELECT 1 FROM variants v JOIN products p ON p.shopify_id=v.product_id WHERE v.inventory_quantity>0 AND `+catalogProductCondition+`)`,
		)
		args = append(args, purchaseProductArgs...)
		args = append(args, catalogProductArgs...)
	default:
		return Result{}, fmt.Errorf("unsupported segment kind %q", definition.Kind)
	}
	if definition.ExcludeProductHandle != "" || definition.ExcludeProductTitle != "" || definition.ExcludeProductTag != "" {
		exclusion, exclusionArgs := productCondition(
			"p", "ol", definition.ExcludeProductHandle, definition.ExcludeProductTitle, definition.ExcludeProductTag,
		)
		where = append(where, `NOT EXISTS (SELECT 1 FROM orders o JOIN order_lines ol ON ol.order_id=o.shopify_id LEFT JOIN products p ON p.shopify_id=ol.product_id WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND `+exclusion+`)`)
		args = append(args, exclusionArgs...)
	}
	if definition.ExcludeRecentDays > 0 {
		where = append(where, `NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id=c.id AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND o.processed_at>=?)`)
		args = append(args, now.AddDate(0, 0, -definition.ExcludeRecentDays).UTC().Format(time.RFC3339))
	}
	rows, err := db.QueryContext(ctx, `SELECT c.id FROM customers c WHERE `+strings.Join(where, " AND ")+` ORDER BY c.id`, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	result := Result{Definition: definition, Exclusions: map[string]int64{}}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return Result{}, err
		}
		result.CustomerIDs = append(result.CustomerIDs, id)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	result.EligibleCount = len(result.CustomerIDs)
	var excluded int64
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM customers WHERE suppressed_at IS NOT NULL OR invalid_number=1`).Scan(&excluded)
	result.Exclusions["suppressed_or_invalid"] = excluded
	if definition.RequireConsent {
		excluded = 0
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM customers WHERE whatsapp_consent!='opted_in'`).Scan(&excluded)
		result.Exclusions["without_whatsapp_consent"] = excluded
	}
	return result, nil
}

func (d Definition) Validate() error {
	switch d.Kind {
	case "not_reordered":
	case "product_buyers", "back_in_stock":
		if d.ProductHandle == "" && d.ProductTitle == "" {
			return errors.New("product_handle or product_title is required")
		}
	case "recent_purchasers":
		if d.WithinDays < 1 {
			return errors.New("within_days must be positive")
		}
	case "lapsed_customers":
		if d.LapsedDays < 1 {
			return errors.New("lapsed_days must be positive")
		}
	default:
		return fmt.Errorf("unsupported segment kind %q", d.Kind)
	}
	if d.ExcludeRecentDays < 0 {
		return errors.New("exclude_recent_purchase_days cannot be negative")
	}
	return nil
}

func productCondition(productAlias, lineAlias, handle, title, tag string) (string, []any) {
	parts := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if handle != "" {
		parts = append(parts, productAlias+".handle=?")
		args = append(args, handle)
	}
	if title != "" {
		pattern := "%" + strings.ToLower(title) + "%"
		parts = append(parts, "lower("+productAlias+".title) LIKE ?")
		args = append(args, pattern)
		if lineAlias != "" {
			parts = append(parts, "lower("+lineAlias+".title) LIKE ?")
			args = append(args, pattern)
		}
	}
	if tag != "" {
		parts = append(parts, "lower("+productAlias+".tags_json) LIKE ?")
		args = append(args, "%"+strings.ToLower(tag)+"%")
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}
