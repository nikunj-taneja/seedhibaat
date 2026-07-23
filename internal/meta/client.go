package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL       string
	APIVersion    string
	AccessToken   string
	WABAID        string
	PhoneNumberID string
	HTTP          *http.Client
}

type APIError struct {
	StatusCode int
	Code       int
	Subcode    int
	Message    string
	Transient  bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Meta API %d (%d/%d): %s", e.StatusCode, e.Code, e.Subcode, e.Message)
}
func (e *APIError) Retryable() bool { return e.Transient || e.StatusCode == 429 || e.StatusCode >= 500 }

type NetworkError struct{ Err error }

func (e *NetworkError) Error() string {
	return "Meta network request failed with unknown acceptance state: " + e.Err.Error()
}
func (e *NetworkError) Unwrap() error { return e.Err }

type PhoneIdentity struct {
	ID                     string `json:"id"`
	DisplayPhoneNumber     string `json:"display_phone_number"`
	VerifiedName           string `json:"verified_name"`
	QualityRating          string `json:"quality_rating"`
	CodeVerificationStatus string `json:"code_verification_status"`
}

type Template struct {
	ID         string      `json:"id,omitempty"`
	Name       string      `json:"name"`
	Language   string      `json:"language"`
	Category   string      `json:"category"`
	Status     string      `json:"status,omitempty"`
	Components []Component `json:"components"`
}

type Component struct {
	Type    string           `json:"type"`
	Format  string           `json:"format,omitempty"`
	Text    string           `json:"text,omitempty"`
	Example any              `json:"example,omitempty"`
	Buttons []TemplateButton `json:"buttons,omitempty"`
}

type TemplateButton struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	URL     string   `json:"url,omitempty"`
	Example []string `json:"example,omitempty"`
}

type MessageRequest struct {
	MessagingProduct string          `json:"messaging_product"`
	RecipientType    string          `json:"recipient_type"`
	To               string          `json:"to"`
	Type             string          `json:"type"`
	Template         MessageTemplate `json:"template"`
}

type MessageTemplate struct {
	Name       string             `json:"name"`
	Language   MessageLanguage    `json:"language"`
	Components []MessageComponent `json:"components,omitempty"`
}

type MessageLanguage struct {
	Code string `json:"code"`
}

type MessageComponent struct {
	Type       string             `json:"type"`
	SubType    string             `json:"sub_type,omitempty"`
	Index      string             `json:"index,omitempty"`
	Parameters []MessageParameter `json:"parameters,omitempty"`
}

type MessageParameter struct {
	Type  string        `json:"type"`
	Text  string        `json:"text,omitempty"`
	Image *MessageMedia `json:"image,omitempty"`
}

type MessageMedia struct {
	Link string `json:"link"`
}

func NewClient(version, token, wabaID, phoneID string) *Client {
	return &Client{
		BaseURL:       "https://graph.facebook.com",
		APIVersion:    version,
		AccessToken:   token,
		WABAID:        wabaID,
		PhoneNumberID: phoneID,
		HTTP:          &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) PhoneIdentity(ctx context.Context, phoneID string) (PhoneIdentity, error) {
	if phoneID == "" {
		phoneID = c.PhoneNumberID
	}
	var result PhoneIdentity
	err := c.do(ctx, http.MethodGet, phoneID, url.Values{"fields": {"id,display_phone_number,verified_name,quality_rating,code_verification_status"}}, nil, &result)
	return result, err
}

func (c *Client) ListPhoneNumbers(ctx context.Context, wabaID string) ([]PhoneIdentity, error) {
	if wabaID == "" {
		wabaID = c.WABAID
	}
	var result struct {
		Data []PhoneIdentity `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, wabaID+"/phone_numbers", url.Values{"fields": {"id,display_phone_number,verified_name,quality_rating,code_verification_status"}}, nil, &result)
	return result.Data, err
}

func (c *Client) SubmitTemplate(ctx context.Context, template Template) (string, error) {
	if strings.ToUpper(template.Category) != "MARKETING" && strings.ToUpper(template.Category) != "UTILITY" {
		return "", errors.New("template category must be MARKETING or UTILITY")
	}
	var result struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, c.WABAID+"/message_templates", nil, template, &result)
	return result.ID, err
}

func (c *Client) Templates(ctx context.Context, names []string) ([]Template, error) {
	values := url.Values{"fields": {"id,name,status,category,language,components"}, "limit": {"250"}}
	if len(names) == 1 {
		values.Set("name", names[0])
	}
	var result struct {
		Data []Template `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, c.WABAID+"/message_templates", values, nil, &result)
	if err != nil {
		return nil, err
	}
	if len(names) < 2 {
		return result.Data, nil
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	filtered := make([]Template, 0, len(names))
	for _, template := range result.Data {
		if wanted[template.Name] {
			filtered = append(filtered, template)
		}
	}
	return filtered, nil
}

func (c *Client) SendTemplate(ctx context.Context, to, name, language string, components []MessageComponent) (string, error) {
	request := MessageRequest{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               to,
		Type:             "template",
		Template:         MessageTemplate{Name: name, Language: MessageLanguage{Code: language}, Components: components},
	}
	var result struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := c.do(ctx, http.MethodPost, c.PhoneNumberID+"/messages", nil, request, &result); err != nil {
		return "", err
	}
	if len(result.Messages) != 1 || result.Messages[0].ID == "" {
		return "", errors.New("Meta accepted no message ID")
	}
	return result.Messages[0].ID, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, requestBody, responseBody any) error {
	if c.AccessToken == "" {
		return errors.New("Meta access token is not configured")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + strings.Trim(c.APIVersion, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return &NetworkError{Err: err}
	}
	defer response.Body.Close()
	responseBytes, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			Error struct {
				Message   string `json:"message"`
				Type      string `json:"type"`
				Code      int    `json:"code"`
				Subcode   int    `json:"error_subcode"`
				Transient bool   `json:"is_transient"`
			} `json:"error"`
		}
		if json.Unmarshal(responseBytes, &apiError) == nil && apiError.Error.Message != "" {
			return &APIError{StatusCode: response.StatusCode, Code: apiError.Error.Code, Subcode: apiError.Error.Subcode, Message: apiError.Error.Message, Transient: apiError.Error.Transient}
		}
		return fmt.Errorf("Meta API HTTP %d", response.StatusCode)
	}
	if responseBody != nil && len(responseBytes) > 0 {
		if err := json.Unmarshal(responseBytes, responseBody); err != nil {
			return fmt.Errorf("decode Meta response: %w", err)
		}
	}
	return nil
}
