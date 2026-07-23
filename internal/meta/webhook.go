package meta

import (
	"encoding/json"
	"errors"
)

type Webhook struct {
	Object string         `json:"object"`
	Entry  []WebhookEntry `json:"entry"`
}

type WebhookEntry struct {
	ID      string          `json:"id"`
	Changes []WebhookChange `json:"changes"`
}

type WebhookChange struct {
	Field string       `json:"field"`
	Value WebhookValue `json:"value"`
}

type WebhookValue struct {
	MessagingProduct string           `json:"messaging_product"`
	Metadata         WebhookMetadata  `json:"metadata"`
	Contacts         []WebhookContact `json:"contacts"`
	Messages         []InboundMessage `json:"messages"`
	Statuses         []MessageStatus  `json:"statuses"`
}

type WebhookMetadata struct {
	DisplayPhoneNumber string `json:"display_phone_number"`
	PhoneNumberID      string `json:"phone_number_id"`
}

type WebhookContact struct {
	WaID    string `json:"wa_id"`
	Profile struct {
		Name string `json:"name"`
	} `json:"profile"`
}

type InboundMessage struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Context   *struct {
		ID string `json:"id"`
	} `json:"context,omitempty"`
	Text *struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`
	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button,omitempty"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply,omitempty"`
		ListReply *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"list_reply,omitempty"`
	} `json:"interactive,omitempty"`
}

type MessageStatus struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
	RecipientID  string `json:"recipient_id"`
	Conversation *struct {
		ID     string `json:"id"`
		Origin struct {
			Type string `json:"type"`
		} `json:"origin"`
	} `json:"conversation,omitempty"`
	Errors []struct {
		Code      int    `json:"code"`
		Title     string `json:"title"`
		Message   string `json:"message"`
		ErrorData struct {
			Details string `json:"details"`
		} `json:"error_data"`
	} `json:"errors,omitempty"`
}

func ParseWebhook(body []byte) (Webhook, error) {
	var webhook Webhook
	if err := json.Unmarshal(body, &webhook); err != nil {
		return Webhook{}, err
	}
	if webhook.Object != "whatsapp_business_account" {
		return Webhook{}, errors.New("unexpected Meta webhook object")
	}
	return webhook, nil
}

func (message InboundMessage) Body() string {
	if message.Text != nil {
		return message.Text.Body
	}
	if message.Button != nil {
		if message.Button.Payload != "" {
			return message.Button.Payload
		}
		return message.Button.Text
	}
	if message.Interactive != nil {
		if message.Interactive.ButtonReply != nil {
			if message.Interactive.ButtonReply.ID != "" {
				return message.Interactive.ButtonReply.ID
			}
			return message.Interactive.ButtonReply.Title
		}
		if message.Interactive.ListReply != nil {
			if message.Interactive.ListReply.ID != "" {
				return message.Interactive.ListReply.ID
			}
			return message.Interactive.ListReply.Title
		}
	}
	return ""
}
