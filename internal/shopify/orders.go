package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const ordersQuery = `query SeedhiBaatOrders($first: Int!, $after: String, $query: String!) {
  orders(first: $first, after: $after, query: $query, sortKey: UPDATED_AT) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id name processedAt updatedAt cancelledAt displayFinancialStatus currencyCode
      currentTotalPriceSet { shopMoney { amount currencyCode } }
      customer { id firstName lastName tags defaultPhoneNumber { phoneNumber whatsAppMarketingConsent { state updatedAt } } }
      lineItems(first: 100) { nodes { id title quantity currentQuantity sku
        product { id title handle productType status tags }
		variant { id title sku inventoryQuantity inventoryItem { id } }
      } }
      fulfillments(first: 50) { id status deliveredAt updatedAt
        fulfillmentLineItems(first: 100) { nodes { quantity lineItem { id } } }
      }
      refunds { createdAt refundLineItems(first: 100) { nodes { quantity lineItem { id } } } }
      returns(first: 20) { nodes { id status } }
    }
  }
}`

const orderByIDQuery = `query SeedhiBaatOrder($id: ID!) {
  order(id: $id) {
    id name processedAt updatedAt cancelledAt displayFinancialStatus currencyCode
    currentTotalPriceSet { shopMoney { amount currencyCode } }
    customer { id firstName lastName tags defaultPhoneNumber { phoneNumber whatsAppMarketingConsent { state updatedAt } } }
    lineItems(first: 100) { nodes { id title quantity currentQuantity sku
      product { id title handle productType status tags }
		variant { id title sku inventoryQuantity inventoryItem { id } }
    } }
    fulfillments(first: 50) { id status deliveredAt updatedAt
      fulfillmentLineItems(first: 100) { nodes { quantity lineItem { id } } }
    }
    refunds { createdAt refundLineItems(first: 100) { nodes { quantity lineItem { id } } } }
    returns(first: 20) { nodes { id status } }
  }
}`

type OrdersPage struct {
	Orders struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []Order `json:"nodes"`
	} `json:"orders"`
}

type Order struct {
	ID                     string  `json:"id"`
	Name                   string  `json:"name"`
	ProcessedAt            string  `json:"processedAt"`
	UpdatedAt              string  `json:"updatedAt"`
	CancelledAt            *string `json:"cancelledAt"`
	DisplayFinancialStatus string  `json:"displayFinancialStatus"`
	CurrencyCode           string  `json:"currencyCode"`
	CurrentTotalPriceSet   struct {
		ShopMoney Money `json:"shopMoney"`
	} `json:"currentTotalPriceSet"`
	Customer  *Customer `json:"customer"`
	LineItems struct {
		Nodes []LineItem `json:"nodes"`
	} `json:"lineItems"`
	Fulfillments []Fulfillment `json:"fulfillments"`
	Refunds      []Refund      `json:"refunds"`
	Returns      struct {
		Nodes []Return `json:"nodes"`
	} `json:"returns"`
}

type Money struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currencyCode"`
}
type Customer struct {
	ID                 string   `json:"id"`
	FirstName          string   `json:"firstName"`
	LastName           string   `json:"lastName"`
	Phone              string   `json:"phone,omitempty"`
	Tags               []string `json:"tags"`
	DefaultPhoneNumber *struct {
		PhoneNumber              string `json:"phoneNumber"`
		WhatsAppMarketingConsent struct {
			State     string `json:"state"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"whatsAppMarketingConsent"`
	} `json:"defaultPhoneNumber"`
}

func (customer Customer) EffectivePhone() string {
	if customer.DefaultPhoneNumber != nil && customer.DefaultPhoneNumber.PhoneNumber != "" {
		return customer.DefaultPhoneNumber.PhoneNumber
	}
	return customer.Phone
}

func (customer Customer) WhatsAppConsent() (string, string) {
	if customer.DefaultPhoneNumber == nil {
		return "", ""
	}
	consent := customer.DefaultPhoneNumber.WhatsAppMarketingConsent
	return consent.State, consent.UpdatedAt
}

type LineItem struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Quantity        int      `json:"quantity"`
	CurrentQuantity int      `json:"currentQuantity"`
	SKU             string   `json:"sku"`
	Product         *Product `json:"product"`
	Variant         *Variant `json:"variant"`
}
type Product struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Handle      string   `json:"handle"`
	ProductType string   `json:"productType"`
	Status      string   `json:"status"`
	Tags        []string `json:"tags"`
}
type Variant struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	SKU               string `json:"sku"`
	InventoryQuantity int    `json:"inventoryQuantity"`
	InventoryItem     *struct {
		ID string `json:"id"`
	} `json:"inventoryItem"`
}
type Fulfillment struct {
	ID                   string  `json:"id"`
	Status               string  `json:"status"`
	DeliveredAt          *string `json:"deliveredAt"`
	UpdatedAt            string  `json:"updatedAt"`
	FulfillmentLineItems struct {
		Nodes []FulfillmentLine `json:"nodes"`
	} `json:"fulfillmentLineItems"`
}
type FulfillmentLine struct {
	Quantity int `json:"quantity"`
	LineItem struct {
		ID string `json:"id"`
	} `json:"lineItem"`
}
type Refund struct {
	CreatedAt       string `json:"createdAt"`
	RefundLineItems struct {
		Nodes []RefundLine `json:"nodes"`
	} `json:"refundLineItems"`
}
type RefundLine struct {
	Quantity int `json:"quantity"`
	LineItem struct {
		ID string `json:"id"`
	} `json:"lineItem"`
}
type Return struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (c *Client) OrdersUpdatedSince(ctx context.Context, since time.Time, after string) (OrdersPage, error) {
	variables := map[string]any{"first": 50, "after": nil, "query": "updated_at:>=" + since.UTC().Format(time.RFC3339)}
	if after != "" {
		variables["after"] = after
	}
	var page OrdersPage
	err := c.GraphQL(ctx, ordersQuery, variables, &page)
	return page, err
}

func (c *Client) OrderByID(ctx context.Context, id string) (Order, error) {
	var result struct {
		Order *Order `json:"order"`
	}
	if err := c.GraphQL(ctx, orderByIDQuery, map[string]any{"id": id}, &result); err != nil {
		return Order{}, err
	}
	if result.Order == nil {
		return Order{}, fmt.Errorf("Shopify order not found")
	}
	return *result.Order, nil
}

func (order Order) FullyDeliveredAt() *time.Time {
	required := map[string]int{}
	for _, line := range order.LineItems.Nodes {
		if line.CurrentQuantity > 0 {
			required[line.ID] = line.CurrentQuantity
		}
	}
	delivered := map[string]int{}
	var latest time.Time
	for _, fulfillment := range order.Fulfillments {
		if fulfillment.DeliveredAt == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, *fulfillment.DeliveredAt)
		if err != nil {
			continue
		}
		if at.After(latest) {
			latest = at
		}
		for _, line := range fulfillment.FulfillmentLineItems.Nodes {
			delivered[line.LineItem.ID] += line.Quantity
		}
	}
	if len(required) == 0 || latest.IsZero() {
		return nil
	}
	for lineID, quantity := range required {
		if delivered[lineID] < quantity {
			return nil
		}
	}
	return &latest
}

func (order Order) RefundedQuantities() map[string]int {
	result := map[string]int{}
	for _, refund := range order.Refunds {
		for _, line := range refund.RefundLineItems.Nodes {
			result[line.LineItem.ID] += line.Quantity
		}
	}
	return result
}

func AmountMinor(amount string) (int64, error) {
	value, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return 0, err
	}
	return int64(value*100 + 0.5), nil
}

func DecodeWebhookOrder(body []byte) (string, error) {
	var payload struct {
		AdminGraphQLAPIID string      `json:"admin_graphql_api_id"`
		ID                json.Number `json:"id"`
		OrderID           json.Number `json:"order_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	if payload.OrderID != "" {
		return "gid://shopify/Order/" + payload.OrderID.String(), nil
	}
	if strings.HasPrefix(payload.AdminGraphQLAPIID, "gid://shopify/Order/") {
		return payload.AdminGraphQLAPIID, nil
	}
	if payload.ID != "" {
		return "gid://shopify/Order/" + payload.ID.String(), nil
	}
	return "", fmt.Errorf("Shopify webhook has no order ID")
}
