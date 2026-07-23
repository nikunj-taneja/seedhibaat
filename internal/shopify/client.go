package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var shopPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.myshopify\.com$`)

type Client struct {
	ShopDomain  string
	ClientID    string
	Secret      string
	APIVersion  string
	HTTP        *http.Client
	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

type GraphQLError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

type AccessScope struct {
	Handle string `json:"handle"`
}

func NewClient(domain, clientID, secret, version string) (*Client, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !shopPattern.MatchString(domain) {
		return nil, errors.New("invalid myshopify.com domain")
	}
	return &Client{ShopDomain: domain, ClientID: clientID, Secret: secret, APIVersion: version, HTTP: &http.Client{Timeout: 45 * time.Second}}, nil
}

func (c *Client) AccessScopes(ctx context.Context) ([]AccessScope, error) {
	var response struct {
		CurrentAppInstallation struct {
			AccessScopes []AccessScope `json:"accessScopes"`
		} `json:"currentAppInstallation"`
	}
	err := c.GraphQL(ctx, `query { currentAppInstallation { accessScopes { handle } } }`, nil, &response)
	return response.CurrentAppInstallation.AccessScopes, err
}

func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, data any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://%s/admin/api/%s/graphql.json", c.ShopDomain, c.APIVersion)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Shopify-Access-Token", token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("Shopify GraphQL request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Shopify GraphQL HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode Shopify GraphQL envelope: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("Shopify GraphQL: %s", envelope.Errors[0].Message)
	}
	if data != nil && len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, data)
	}
	return nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExpiry) > 5*time.Minute {
		return c.token, nil
	}
	if c.ClientID == "" || c.Secret == "" {
		return "", errors.New("Shopify client credentials are not configured")
	}
	payload, _ := json.Marshal(map[string]string{"client_id": c.ClientID, "client_secret": c.Secret, "grant_type": "client_credentials"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://%s/admin/oauth/access_token", c.ShopDomain), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return "", fmt.Errorf("Shopify token request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Shopify token HTTP %d", response.StatusCode)
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", err
	}
	if tokenResponse.AccessToken == "" {
		return "", errors.New("Shopify returned an empty access token")
	}
	if tokenResponse.ExpiresIn <= 0 {
		tokenResponse.ExpiresIn = 24 * 60 * 60
	}
	c.token = tokenResponse.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	return c.token, nil
}
