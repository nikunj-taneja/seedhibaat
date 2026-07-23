package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const productQuery = `query SeedhiBaatProduct($id: ID!) { product(id:$id) { id title handle productType status tags variants(first:250) { nodes { id title sku inventoryQuantity inventoryItem { id } } } } }`
const inventoryItemQuery = `query SeedhiBaatInventory($id: ID!) { inventoryItem(id:$id) { id variant { id title sku inventoryQuantity product { id title handle productType status tags } } inventoryLevels(first:100) { nodes { id updatedAt location { id } quantities(names:["available"]) { name quantity } } } } }`
const productsUpdatedQuery = `query SeedhiBaatProducts($first: Int!, $after: String, $query: String!) { products(first:$first,after:$after,query:$query,sortKey:UPDATED_AT) { pageInfo { hasNextPage endCursor } nodes { id title handle productType status tags variants(first:250) { nodes { id title sku inventoryQuantity inventoryItem { id } } } } } }`

type ProductDetail struct {
	Product
	Variants struct {
		Nodes []Variant `json:"nodes"`
	} `json:"variants"`
}
type InventoryItem struct {
	ID      string `json:"id"`
	Variant *struct {
		Variant
		Product *Product `json:"product"`
	} `json:"variant"`
	InventoryLevels struct {
		Nodes []InventoryLevel `json:"nodes"`
	} `json:"inventoryLevels"`
}
type InventoryLevel struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"updatedAt"`
	Location  struct {
		ID string `json:"id"`
	} `json:"location"`
	Quantities []struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
	} `json:"quantities"`
}
type ProductsPage struct {
	Products struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []ProductDetail `json:"nodes"`
	} `json:"products"`
}

func (c *Client) ProductByID(ctx context.Context, id string) (ProductDetail, error) {
	var result struct {
		Product *ProductDetail `json:"product"`
	}
	if err := c.GraphQL(ctx, productQuery, map[string]any{"id": id}, &result); err != nil {
		return ProductDetail{}, err
	}
	if result.Product == nil {
		return ProductDetail{}, fmt.Errorf("Shopify product not found")
	}
	return *result.Product, nil
}

func (c *Client) ProductsUpdatedSince(ctx context.Context, since time.Time, after string) (ProductsPage, error) {
	variables := map[string]any{"first": 50, "after": nil, "query": "updated_at:>=" + since.UTC().Format(time.RFC3339)}
	if after != "" {
		variables["after"] = after
	}
	var page ProductsPage
	err := c.GraphQL(ctx, productsUpdatedQuery, variables, &page)
	return page, err
}
func (c *Client) InventoryItemByID(ctx context.Context, id string) (InventoryItem, error) {
	var result struct {
		InventoryItem *InventoryItem `json:"inventoryItem"`
	}
	if err := c.GraphQL(ctx, inventoryItemQuery, map[string]any{"id": id}, &result); err != nil {
		return InventoryItem{}, err
	}
	if result.InventoryItem == nil {
		return InventoryItem{}, fmt.Errorf("Shopify inventory item not found")
	}
	return *result.InventoryItem, nil
}

func DecodeResourceID(body []byte, resource string) (string, error) {
	var payload struct {
		AdminGraphQLAPIID string      `json:"admin_graphql_api_id"`
		ID                json.Number `json:"id"`
		InventoryItemID   json.Number `json:"inventory_item_id"`
		ProductID         json.Number `json:"product_id"`
		CustomerID        json.Number `json:"customer_id"`
		GraphQLCustomerID string      `json:"admin_graphql_api_customer_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if resource == "inventory_item" && payload.InventoryItemID != "" {
		return "gid://shopify/InventoryItem/" + payload.InventoryItemID.String(), nil
	}
	if resource == "product" && payload.ProductID != "" {
		return "gid://shopify/Product/" + payload.ProductID.String(), nil
	}
	if resource == "customer" && payload.GraphQLCustomerID != "" {
		return payload.GraphQLCustomerID, nil
	}
	if resource == "customer" && payload.CustomerID != "" {
		return "gid://shopify/Customer/" + payload.CustomerID.String(), nil
	}
	if payload.AdminGraphQLAPIID != "" {
		return payload.AdminGraphQLAPIID, nil
	}
	if payload.ID != "" {
		typeName := map[string]string{"product": "Product", "inventory_item": "InventoryItem", "customer": "Customer"}[resource]
		if typeName == "" {
			return "", fmt.Errorf("unsupported Shopify resource %q", resource)
		}
		return "gid://shopify/" + typeName + "/" + payload.ID.String(), nil
	}
	return "", fmt.Errorf("Shopify webhook has no %s ID", resource)
}
