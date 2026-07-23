package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func HMACBase64Valid(secret string, body []byte, supplied string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1
}

func HMACSHA256HexValid(secret string, body []byte, supplied string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(supplied, prefix) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := fmt.Sprintf("%x", mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(strings.TrimPrefix(supplied, prefix))) == 1
}

func KeyedHash(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func Encrypt(keyMaterial string, plaintext []byte) ([]byte, error) {
	key := sha256.Sum256([]byte(keyMaterial))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func Decrypt(keyMaterial string, ciphertext []byte) ([]byte, error) {
	key := sha256.Sum256([]byte(keyMaterial))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext is too short")
	}
	nonce, payload := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, payload, nil)
}

func NewTrackedToken(signingKey, recordID string, expiresAt time.Time) string {
	payload := recordID + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	signature := KeyedHash(signingKey, payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + signature
}

func VerifyTrackedToken(signingKey, token string, now time.Time) (string, error) {
	payloadEncoded, signature, ok := strings.Cut(token, ".")
	if !ok {
		return "", errors.New("malformed token")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return "", errors.New("malformed token")
	}
	payload := string(payloadBytes)
	if subtle.ConstantTimeCompare([]byte(KeyedHash(signingKey, payload)), []byte(signature)) != 1 {
		return "", errors.New("invalid signature")
	}
	recordID, expiresText, ok := strings.Cut(payload, ".")
	if !ok || recordID == "" {
		return "", errors.New("malformed token payload")
	}
	expiresUnix, err := strconv.ParseInt(expiresText, 10, 64)
	if err != nil || !now.Before(time.Unix(expiresUnix, 0)) {
		return "", errors.New("expired token")
	}
	return recordID, nil
}

func SafeDestination(raw string, allowedHosts map[string]bool) (*url.URL, error) {
	destination, err := url.Parse(raw)
	if err != nil || destination.Scheme != "https" || destination.User != nil || destination.Hostname() == "" {
		return nil, errors.New("destination must be an absolute HTTPS URL")
	}
	if !allowedHosts[strings.ToLower(destination.Hostname())] {
		return nil, errors.New("destination host is not allowed")
	}
	return destination, nil
}
