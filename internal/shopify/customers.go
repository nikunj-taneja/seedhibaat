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
