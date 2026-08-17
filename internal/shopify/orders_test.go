package shopify

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFullyDeliveredRequiresEveryCurrentLine(t *testing.T) {
	delivered := "2026-07-20T10:00:00Z"
	order := Order{}
	order.LineItems.Nodes = []LineItem{{ID: "a", CurrentQuantity: 1}, {ID: "b", CurrentQuantity: 2}}
	fulfillment := Fulfillment{DeliveredAt: &delivered}
	fulfillment.FulfillmentLineItems.Nodes = []FulfillmentLine{{Quantity: 1}, {Quantity: 1}}
	fulfillment.FulfillmentLineItems.Nodes[0].LineItem.ID = "a"
	fulfillment.FulfillmentLineItems.Nodes[1].LineItem.ID = "b"
	order.Fulfillments = []Fulfillment{fulfillment}
	if order.FullyDeliveredAt() != nil {
		t.Fatal("partial shipment counted as delivered")
	}
	order.Fulfillments[0].FulfillmentLineItems.Nodes[1].Quantity = 2
	at := order.FullyDeliveredAt()
	if at == nil || !at.Equal(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("delivered=%v", at)
	}
}

func TestCustomerUsesChannelSpecificWhatsAppConsent(t *testing.T) {
	var customer Customer
	if err := json.Unmarshal([]byte(`{"id":"gid://shopify/Customer/1","defaultPhoneNumber":{"phoneNumber":"+919999999999","whatsAppMarketingConsent":{"state":"SUBSCRIBED","updatedAt":"2026-07-22T12:00:00Z"}}}`), &customer); err != nil {
		t.Fatal(err)
	}
	state, updatedAt := customer.WhatsAppConsent()
	if customer.EffectivePhone() != "+919999999999" || state != "SUBSCRIBED" || updatedAt != "2026-07-22T12:00:00Z" {
		t.Fatalf("phone=%q state=%q updated=%q", customer.EffectivePhone(), state, updatedAt)
	}
}

func TestDecodeResourceIDUsesCanonicalGraphQLType(t *testing.T) {
	for resource, want := range map[string]string{
		"product":  "gid://shopify/Product/123",
		"customer": "gid://shopify/Customer/123",
	} {
		got, err := DecodeResourceID([]byte(`{"id":123}`), resource)
		if err != nil || got != want {
			t.Fatalf("resource=%s got=%q err=%v", resource, got, err)
		}
	}
}

func TestDecodeWebhookOrderUsesParentOrderForChildTopics(t *testing.T) {
	id, err := DecodeWebhookOrder([]byte(`{"id":222,"order_id":111,"admin_graphql_api_id":"gid://shopify/Fulfillment/222"}`))
	if err != nil {
		t.Fatal(err)
	}
	if id != "gid://shopify/Order/111" {
		t.Fatalf("order id=%q", id)
	}
}

func TestDecodeWebhookOrderAcceptsOrderGraphQLID(t *testing.T) {
	id, err := DecodeWebhookOrder([]byte(`{"id":111,"admin_graphql_api_id":"gid://shopify/Order/111"}`))
	if err != nil {
		t.Fatal(err)
	}
	if id != "gid://shopify/Order/111" {
		t.Fatalf("order id=%q", id)
	}
}

func TestOrderPhoneFallsBackThroughAddresses(t *testing.T) {
	shipping := Order{Phone: "+911111111111"}
	shipping.ShippingAddress = &struct {
		Phone string `json:"phone"`
	}{Phone: "+912222222222"}
	if got := shipping.EffectiveCustomerPhone(); got != "+912222222222" {
		t.Fatalf("shipping address should win: %s", got)
	}

	billing := Order{Phone: "+911111111111"}
	billing.BillingAddress = &struct {
		Phone string `json:"phone"`
	}{Phone: "+913333333333"}
	if got := billing.EffectiveCustomerPhone(); got != "+913333333333" {
		t.Fatalf("billing address should be used next: %s", got)
	}

	if got := (Order{Phone: "+911111111111"}).EffectiveCustomerPhone(); got != "+911111111111" {
		t.Fatalf("order phone is the last resort: %s", got)
	}
	if got := (Order{}).EffectiveCustomerPhone(); got != "" {
		t.Fatalf("no phone anywhere should be empty: %s", got)
	}
}
