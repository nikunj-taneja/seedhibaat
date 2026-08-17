package service

import (
	"context"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/shopify"
)

// Shopify withholds customer-level protected data from this app, so the order
// is the only place the buyer's number appears. A customer with no number can
// never be found by phone, so no send to them could be accounted for.
func TestOrderSyncTakesTheBuyerPhoneFromTheOrder(t *testing.T) {
	processor, db := testProcessor(t)
	defer db.Close()
	order := shopify.Order{
		ID:          "gid://shopify/Order/500",
		Name:        "#500",
		ProcessedAt: time.Now().UTC().Format(time.RFC3339),
		Customer:    &shopify.Customer{ID: "gid://shopify/Customer/500"},
	}
	order.CurrentTotalPriceSet.ShopMoney = shopify.Money{Amount: "749.00", CurrencyCode: "INR"}
	order.ShippingAddress = &struct {
		Phone string `json:"phone"`
	}{Phone: "+919876500500"}
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var storedHash string
	if err := db.DB.QueryRow(`SELECT coalesce(phone_hash,'') FROM customers WHERE shopify_id='gid://shopify/Customer/500'`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != security.KeyedHash(processor.Config.PIIHashKey, "919876500500") {
		t.Fatalf("order phone was not attached to the customer: %q", storedHash)
	}
}
