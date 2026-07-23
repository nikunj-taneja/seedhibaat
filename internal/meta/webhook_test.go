package meta

import (
	"encoding/json"
	"testing"
)

func TestInboundButtonBodyUsesStableProviderID(t *testing.T) {
	for _, fixture := range []struct {
		body string
		want string
	}{
		{`{"type":"button","button":{"text":"Stop messages","payload":"STOP"}}`, "STOP"},
		{`{"type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"view_product","title":"View Product"}}}`, "view_product"},
		{`{"type":"interactive","interactive":{"type":"list_reply","list_reply":{"id":"product_one","title":"Product One"}}}`, "product_one"},
	} {
		var message InboundMessage
		if err := json.Unmarshal([]byte(fixture.body), &message); err != nil {
			t.Fatal(err)
		}
		if got := message.Body(); got != fixture.want {
			t.Fatalf("got=%q want=%q", got, fixture.want)
		}
	}
}
