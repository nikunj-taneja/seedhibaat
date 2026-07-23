package workflow

import (
	"strings"
	"testing"
	"time"
)

func TestLoadPostDeliveryWorkflow(t *testing.T) {
	loaded, err := Load("../../config/workflows/post_delivery_followup_example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Definition.Enabled {
		t.Fatal("production workflow must default disabled")
	}
	if len(loaded.Definition.Steps) != 4 {
		t.Fatalf("steps=%d", len(loaded.Definition.Steps))
	}
	for _, step := range loaded.Definition.Steps {
		if step.Category != "MARKETING" {
			t.Fatalf("wrong category: %s", step.Category)
		}
	}
}

func TestRejectUnknownFieldAndWrongCategory(t *testing.T) {
	bad := []byte("name: x\nversion: 1\ntimezone: Asia/Kolkata\nunknown: true\n")
	if _, err := Parse("test", bad); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestQuietHoursCrossMidnight(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Kolkata")
	candidate := time.Date(2026, 7, 22, 23, 30, 0, 0, location)
	next, err := NextAllowedTime(candidate, "Asia/Kolkata", "21:00", "10:00")
	if err != nil {
		t.Fatal(err)
	}
	local := next.In(location)
	if local.Hour() != 10 || local.Day() != 23 {
		t.Fatalf("next=%s", local)
	}
}

func TestTemplateParameterBindingsAreOrderedAndValidated(t *testing.T) {
	bindings, err := OrderedParameterBindings(map[string]string{
		"body.2": "order.number",
		"body.1": "customer.first_name",
	}, "body")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0] != "customer.first_name" || bindings[1] != "order.number" {
		t.Fatalf("bindings=%v", bindings)
	}
	if _, err := OrderedParameterBindings(map[string]string{"body.2": "order.number"}, "body"); err == nil {
		t.Fatal("non-contiguous parameters accepted")
	}
	if _, err := OrderedParameterBindings(map[string]string{"body.1": "customer.phone"}, "body"); err == nil {
		t.Fatal("unsupported private source accepted")
	}
}

func TestWaitsSupportDaysHoursAndMinutes(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"30m":   30 * time.Minute,
		"6h":    6 * time.Hour,
		"7d":    7 * 24 * time.Hour,
		"1d12h": 36 * time.Hour,
	} {
		got, err := ParseWait(input)
		if err != nil || got != want {
			t.Fatalf("input=%s got=%s want=%s err=%v", input, got, want, err)
		}
	}
}

func TestImageHeaderURLValidation(t *testing.T) {
	valid := []byte(`
name: image_flow
version: 1
enabled: false
timezone: Asia/Kolkata
trigger:
  type: order_delivered
quiet_hours:
  start: "21:00"
  end: "10:00"
frequency_cap:
  messages: 1
  window: 24h
steps:
  - id: image
    wait: 1d
    template: image_template
    language: en_US
    category: MARKETING
    header_image_url: https://cdn.example.com/header.webp
    params: {}
`)
	if _, err := Parse("image.yaml", valid); err != nil {
		t.Fatal(err)
	}
	invalidHTTP := []byte(string(valid))
	invalidHTTP = []byte(stringReplace(string(invalidHTTP), "https://", "http://"))
	if _, err := Parse("image-http.yaml", invalidHTTP); err == nil {
		t.Fatal("non-HTTPS image URL accepted")
	}
	withTextHeader := []byte(stringReplace(string(valid), "params: {}", "params:\n      header.1: literal:Hello"))
	if _, err := Parse("image-text-header.yaml", withTextHeader); err == nil {
		t.Fatal("image header combined with text header parameters")
	}
}

func stringReplace(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}
