// One-off repair: remove outbound_messages rows recorded with source='cli'
// and any phone-only customer rows those records created.
//
// A phone-only customer row (no shopify_id) collides with the Shopify customer
// upsert, which infers ON CONFLICT(shopify_id) only; SQLite aborts the
// statement on a phone_hash conflict, so that customer's orders stop syncing.
//
// Dry run by default. Prints counts only, never PII.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	apply := flag.Bool("apply", false, "perform the deletion instead of reporting it")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Println("usage: cleanup-cli-records [--apply] <database>")
		os.Exit(2)
	}
	db, err := sql.Open("sqlite", flag.Arg(0)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		panic(err)
	}
	defer tx.Rollback()

	var messages int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbound_messages WHERE source='cli'`).Scan(&messages); err != nil {
		panic(err)
	}

	// Only customers that this recorder could have created, and only when no
	// other record depends on them once the CLI messages are gone.
	const orphanFilter = `
		FROM customers c
		WHERE c.shopify_id IS NULL
		  AND c.whatsapp_consent='unknown'
		  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id=c.id)
		  AND NOT EXISTS (SELECT 1 FROM workflow_runs w WHERE w.customer_id=c.id)
		  AND NOT EXISTS (SELECT 1 FROM campaign_recipients r WHERE r.customer_id=c.id)
		  AND NOT EXISTS (SELECT 1 FROM replies p WHERE p.customer_id=c.id)
		  AND NOT EXISTS (SELECT 1 FROM frequency_caps f WHERE f.customer_id=c.id)
		  AND NOT EXISTS (SELECT 1 FROM outbound_messages m WHERE m.customer_id=c.id AND m.source<>'cli')`

	var orphans, retained int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) `+orphanFilter).Scan(&orphans); err != nil {
		panic(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM customers WHERE shopify_id IS NULL AND whatsapp_consent='unknown'`).Scan(&retained); err != nil {
		panic(err)
	}

	fmt.Printf("outbound_messages with source='cli': %d\n", messages)
	fmt.Printf("phone-only customers, deletable:     %d\n", orphans)
	fmt.Printf("phone-only customers, kept (in use): %d\n", retained-orphans)

	if !*apply {
		fmt.Println("\nDry run. Re-run with --apply to delete.")
		return
	}

	deletedMessages, err := tx.ExecContext(ctx, `DELETE FROM message_events WHERE message_id IN (SELECT id FROM outbound_messages WHERE source='cli')`)
	if err != nil {
		panic(err)
	}
	events, _ := deletedMessages.RowsAffected()
	result, err := tx.ExecContext(ctx, `DELETE FROM outbound_messages WHERE source='cli'`)
	if err != nil {
		panic(err)
	}
	removedMessages, _ := result.RowsAffected()
	result, err = tx.ExecContext(ctx, `DELETE FROM customers WHERE id IN (SELECT c.id `+orphanFilter+`)`)
	if err != nil {
		panic(err)
	}
	removedCustomers, _ := result.RowsAffected()

	if err := tx.Commit(); err != nil {
		panic(err)
	}
	fmt.Printf("\ndeleted message_events: %d\ndeleted messages: %d\ndeleted customers: %d\n", events, removedMessages, removedCustomers)
}
