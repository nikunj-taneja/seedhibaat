package segment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

func TestProductBuyerSegmentExcludesConversionAndSuppression(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for id := 1; id <= 3; id++ {
		_, err = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, "c"+string(rune('0'+id)), []byte("phone"), "hash"+string(rune('0'+id)), "opted_in", now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _ = db.DB.Exec(`UPDATE customers SET suppressed_at=?,suppression_reason='test' WHERE id=3`, now)
	_, _ = db.DB.Exec(`INSERT INTO products(shopify_id,title,handle,tags_json,updated_at) VALUES('starter','Starter Product','starter-product','["starter"]',?),('upgrade','Upgraded Product','upgraded-product','["upgrade"]',?)`, now, now)
	for id := 1; id <= 3; id++ {
		order := "o" + string(rune('0'+id))
		_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,created_at,updated_at) VALUES(?,?,?,?)`, order, id, now, now)
		_, _ = db.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,product_id,title,quantity,current_quantity) VALUES(?,?,?,?,1,1)`, "l"+string(rune('0'+id)), order, "starter", "Starter Product")
	}
	_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,created_at,updated_at) VALUES('o100',2,?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,product_id,title,quantity,current_quantity) VALUES('upgrade-line','o100','upgrade','Upgraded Product',1,1)`)
	result, err := Preview(context.Background(), db.DB, Definition{Kind: "product_buyers", ProductHandle: "starter-product", RequireConsent: true, ExcludeProductTag: "upgrade"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.EligibleCount != 1 || len(result.CustomerIDs) != 1 || result.CustomerIDs[0] != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestPurchaseSegmentsExcludeRefundedAndReturnedOrders(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for id := 1; id <= 2; id++ {
		_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(?,?,x'01','opted_in',?,?)`, id, fmt.Sprintf("customer-%d", id), now, now)
	}
	_, _ = db.DB.Exec(`INSERT INTO products(shopify_id,title,handle,updated_at) VALUES('product','Starter Product','starter-product',?)`, now)
	_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,refunded_at,created_at,updated_at) VALUES('refunded',1,?,?,?)`, now, now, now)
	_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,return_recorded_at,created_at,updated_at) VALUES('returned',2,?,?,?)`, now, now, now)
	_, _ = db.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,product_id,title,quantity,current_quantity) VALUES('line-1','refunded','product','Starter Product',1,1),('line-2','returned','product','Starter Product',1,1)`)
	result, err := Preview(context.Background(), db.DB, Definition{Kind: "product_buyers", ProductHandle: "starter-product", RequireConsent: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.EligibleCount != 0 {
		t.Fatalf("eligible refunded/returned customers=%d", result.EligibleCount)
	}
}

func TestFrozenCSVUsesShopifyIDsAndAppliesSendTimeExclusions(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for id := 1; id <= 4; id++ {
		_, err = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(?,?,x'01','opted_in',?,?)`, id, fmt.Sprintf("gid://shopify/Customer/%d", id), now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _ = db.DB.Exec(`UPDATE customers SET whatsapp_consent='opted_out' WHERE id=2`)
	_, _ = db.DB.Exec(`UPDATE customers SET suppressed_at=? WHERE id=3`, now)
	_, _ = db.DB.Exec(`UPDATE customers SET invalid_number=1 WHERE id=4`)
	definition := Definition{Kind: "frozen_csv", RequireConsent: true, CustomerShopifyIDs: []string{"gid://shopify/Customer/1", "gid://shopify/Customer/2", "gid://shopify/Customer/3", "gid://shopify/Customer/4"}}
	result, err := Preview(context.Background(), db.DB, definition, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.EligibleCount != 1 || len(result.CustomerIDs) != 1 || result.CustomerIDs[0] != 1 {
		t.Fatalf("result=%+v", result)
	}
	if result.Definition.FrozenCount != 4 || result.Definition.CustomerShopifyIDs != nil {
		t.Fatalf("public definition leaked IDs: %+v", result.Definition)
	}
}

func TestFrozenCSVRejectsDuplicateAndMalformedIDs(t *testing.T) {
	duplicate := Definition{Kind: "frozen_csv", RequireConsent: true, CustomerShopifyIDs: []string{"gid://shopify/Customer/1", "gid://shopify/Customer/1"}}
	if duplicate.Validate() == nil {
		t.Fatal("expected duplicate validation error")
	}
	malformed := Definition{Kind: "frozen_csv", RequireConsent: true, CustomerShopifyIDs: []string{"1"}}
	if malformed.Validate() == nil {
		t.Fatal("expected malformed validation error")
	}
}
