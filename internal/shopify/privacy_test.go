package shopify

import "testing"

func TestDecodePrivacyRequest(t *testing.T) {
	request, err := DecodePrivacyRequest([]byte(`{"shop_domain":"example.myshopify.com","customer":{"id":123},"orders_to_redact":[456],"orders_requested":[789]}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.CustomerID != "gid://shopify/Customer/123" || len(request.OrderIDs) != 1 || request.OrderIDs[0] != "gid://shopify/Order/456" || len(request.OrdersRequested) != 1 {
		t.Fatalf("request=%+v", request)
	}
}

func TestDecodePrivacyRequestRequiresShop(t *testing.T) {
	if _, err := DecodePrivacyRequest([]byte(`{"customer":{"id":123}}`)); err == nil {
		t.Fatal("missing shop domain accepted")
	}
}
