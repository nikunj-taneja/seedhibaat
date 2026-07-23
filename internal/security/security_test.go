package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestSignatures(t *testing.T) {
	body := []byte(`{"ok":true}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	if !HMACBase64Valid("secret", body, base64.StdEncoding.EncodeToString(mac.Sum(nil))) {
		t.Fatal("Shopify signature rejected")
	}
	if !HMACSHA256HexValid("secret", body, "sha256="+fmt.Sprintf("%x", mac.Sum(nil))) {
		t.Fatal("Meta signature rejected")
	}
	if HMACBase64Valid("wrong", body, base64.StdEncoding.EncodeToString(mac.Sum(nil))) {
		t.Fatal("wrong secret accepted")
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	ciphertext, err := Encrypt("key", []byte("private"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "private" {
		t.Fatal("plaintext was not encrypted")
	}
	plaintext, err := Decrypt("key", ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "private" {
		t.Fatalf("got %q", plaintext)
	}
}

func TestTrackedTokenAndSafeDestination(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token := NewTrackedToken("key", "record", now.Add(time.Hour))
	record, err := VerifyTrackedToken("key", token, now)
	if err != nil || record != "record" {
		t.Fatalf("verify: %q %v", record, err)
	}
	if _, err := VerifyTrackedToken("key", token, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired token accepted")
	}
	if _, err := SafeDestination("https://shop.example.com/products/x", map[string]bool{"shop.example.com": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeDestination("https://evil.example/x", map[string]bool{"shop.example.com": true}); err == nil {
		t.Fatal("open redirect accepted")
	}
}
