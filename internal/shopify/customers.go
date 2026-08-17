package shopify

import (
	"context"
	"fmt"
	"time"
)

const customerByIDQuery = `query SeedhiBaatCustomer($id: ID!) {
  customer(id: $id) {
    id firstName lastName tags
    defaultPhoneNumber { phoneNumber whatsAppMarketingConsent { state updatedAt } }
  }
}`

const customersUpdatedQuery = `query SeedhiBaatCustomers($first: Int!, $after: String, $query: String!) {
  customers(first: $first, after: $after, query: $query, sortKey: UPDATED_AT) {
    pageInfo { hasNextPage endCursor }
    nodes { id firstName lastName tags defaultPhoneNumber { phoneNumber whatsAppMarketingConsent { state updatedAt } } }
  }
}`

type CustomersPage struct {
	Customers struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []Customer `json:"nodes"`
	} `json:"customers"`
}

func (c *Client) CustomerByID(ctx context.Context, id string) (Customer, error) {
	var result struct {
		Customer *Customer `json:"customer"`
	}
	if err := c.GraphQL(ctx, customerByIDQuery, map[string]any{"id": id}, &result); err != nil {
		return Customer{}, err
	}
	if result.Customer == nil {
		return Customer{}, fmt.Errorf("Shopify customer not found")
	}
	return *result.Customer, nil
}

func (c *Client) CustomersUpdatedSince(ctx context.Context, since time.Time, after string) (CustomersPage, error) {
	variables := map[string]any{"first": 50, "after": nil, "query": "updated_at:>=" + since.UTC().Format(time.RFC3339)}
	if after != "" {
		variables["after"] = after
	}
	var page CustomersPage
	err := c.GraphQL(ctx, customersUpdatedQuery, variables, &page)
	return page, err
}

const customerOrderPhoneQuery = `query SeedhiBaatCustomerOrderPhone($id: ID!) {
  customer(id: $id) {
    orders(first: 1, sortKey: PROCESSED_AT, reverse: true) {
      nodes { id phone shippingAddress { phone } billingAddress { phone } }
    }
  }
}`

// CustomerOrderPhone reads a customer's number from their most recent order.
// Shopify returns null for customer-level protected data unless the app is
// approved for it, while the same number stays readable on the order.
func (c *Client) CustomerOrderPhone(ctx context.Context, customerID string) (string, error) {
	var result struct {
		Customer *struct {
			Orders struct {
				Nodes []Order `json:"nodes"`
			} `json:"orders"`
		} `json:"customer"`
	}
	if err := c.GraphQL(ctx, customerOrderPhoneQuery, map[string]any{"id": customerID}, &result); err != nil {
		return "", err
	}
	if result.Customer == nil || len(result.Customer.Orders.Nodes) == 0 {
		return "", nil
	}
	return result.Customer.Orders.Nodes[0].EffectiveCustomerPhone(), nil
}
