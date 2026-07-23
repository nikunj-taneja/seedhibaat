package shopify

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type PrivacyRequest struct {
	ShopDomain      string
	CustomerID      string
	OrderIDs        []string
	OrdersRequested []string
}

func DecodePrivacyRequest(body []byte) (PrivacyRequest, error) {
	var payload struct {
		ShopDomain string `json:"shop_domain"`
		Customer   struct {
			ID json.Number `json:"id"`
		} `json:"customer"`
		OrdersToRedact  []json.Number `json:"orders_to_redact"`
		OrdersRequested []json.Number `json:"orders_requested"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return PrivacyRequest{}, err
	}
	request := PrivacyRequest{ShopDomain: payload.ShopDomain}
	if payload.Customer.ID != "" {
		request.CustomerID = "gid://shopify/Customer/" + payload.Customer.ID.String()
	}
	for _, id := range payload.OrdersToRedact {
		request.OrderIDs = append(request.OrderIDs, "gid://shopify/Order/"+id.String())
	}
	for _, id := range payload.OrdersRequested {
		request.OrdersRequested = append(request.OrdersRequested, "gid://shopify/Order/"+id.String())
	}
	if request.ShopDomain == "" {
		return PrivacyRequest{}, fmt.Errorf("Shopify privacy webhook has no shop domain")
	}
	return request, nil
}
