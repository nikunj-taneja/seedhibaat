package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Definition struct {
	Name        string       `yaml:"name"`
	Version     int          `yaml:"version"`
	Description string       `yaml:"description"`
	Enabled     bool         `yaml:"enabled"`
	Timezone    string       `yaml:"timezone"`
	Trigger     Trigger      `yaml:"trigger"`
	Audience    Audience     `yaml:"audience"`
	QuietHours  QuietHours   `yaml:"quiet_hours"`
	Frequency   FrequencyCap `yaml:"frequency_cap"`
	Conversion  Conversion   `yaml:"conversion"`
	Steps       []Step       `yaml:"steps"`
}

type Trigger struct {
	Type string `yaml:"type"`
}

type Audience struct {
	ProductHandles []string `yaml:"product_handles"`
	ProductTags    []string `yaml:"product_tags"`
	ProductTitles  []string `yaml:"product_titles"`
	RequireConsent bool     `yaml:"require_whatsapp_consent"`
}

type QuietHours struct {
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

type FrequencyCap struct {
	Messages int    `yaml:"messages"`
	Window   string `yaml:"window"`
}

type Conversion struct {
	ProductTags    []string `yaml:"product_tags"`
	ProductTitles  []string `yaml:"product_titles"`
	ProductHandles []string `yaml:"product_handles"`
}

type Step struct {
	ID         string            `yaml:"id"`
	Wait       string            `yaml:"wait"`
	Template   string            `yaml:"template"`
	Language   string            `yaml:"language"`
	Category   string            `yaml:"category"`
	Params     map[string]string `yaml:"params"`
	URL        string            `yaml:"tracked_url"`
	Conditions Conditions        `yaml:"conditions"`
}

type Conditions struct {
	OrderNotCancelled       bool              `yaml:"order_not_cancelled" json:"order_not_cancelled,omitempty"`
	OrderNotRefunded        bool              `yaml:"order_not_refunded" json:"order_not_refunded,omitempty"`
	CustomerHasPurchased    *ProductCondition `yaml:"customer_has_purchased" json:"customer_has_purchased,omitempty"`
	CustomerHasNotPurchased *ProductCondition `yaml:"customer_has_not_purchased" json:"customer_has_not_purchased,omitempty"`
}

type ProductCondition struct {
	ProductHandles []string `yaml:"product_handles" json:"product_handles,omitempty"`
	ProductTags    []string `yaml:"product_tags" json:"product_tags,omitempty"`
	ProductTitles  []string `yaml:"product_titles" json:"product_titles,omitempty"`
}

type Loaded struct {
	Definition Definition
	YAML       []byte
	Hash       string
	Path       string
}

func LoadDir(directory string) ([]Loaded, error) {
	var paths []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(paths)
	result := make([]Loaded, 0, len(paths))
	for _, path := range paths {
		loaded, err := Load(path)
		if err != nil {
			return nil, err
		}
		result = append(result, loaded)
	}
	return result, nil
}

func Load(path string) (Loaded, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Loaded{}, err
	}
	return Parse(path, body)
}

func Parse(source string, body []byte) (Loaded, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var definition Definition
	if err := decoder.Decode(&definition); err != nil {
		return Loaded{}, fmt.Errorf("parse workflow %s: %w", source, err)
	}
	if err := definition.Validate(); err != nil {
		return Loaded{}, fmt.Errorf("validate workflow %s: %w", source, err)
	}
	digest := sha256.Sum256(body)
	return Loaded{Definition: definition, YAML: body, Hash: hex.EncodeToString(digest[:]), Path: source}, nil
}

func (d Definition) Validate() error {
	if d.Name == "" || d.Version < 1 {
		return errors.New("name and positive version are required")
	}
	if d.Enabled {
		return errors.New("enabled must remain false; activate only through the confirmed-count operator command")
	}
	if d.Timezone == "" {
		return errors.New("timezone is required")
	}
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		return fmt.Errorf("invalid timezone: %w", err)
	}
	switch d.Trigger.Type {
	case "order_delivered", "inventory_back_in_stock", "manual_campaign":
	default:
		return fmt.Errorf("unsupported trigger type %q", d.Trigger.Type)
	}
	if len(d.Steps) == 0 {
		return errors.New("at least one step is required")
	}
	seen := map[string]bool{}
	for _, step := range d.Steps {
		if step.ID == "" || seen[step.ID] {
			return fmt.Errorf("step IDs must be non-empty and unique: %q", step.ID)
		}
		seen[step.ID] = true
		if _, err := ParseWait(step.Wait); err != nil {
			return fmt.Errorf("step %s has invalid wait: %w", step.ID, err)
		}
		if step.Template == "" {
			return fmt.Errorf("step %s has no template", step.ID)
		}
		if step.Language == "" {
			return fmt.Errorf("step %s has no language", step.ID)
		}
		if strings.ToUpper(step.Category) != "MARKETING" {
			return fmt.Errorf("step %s must be explicitly categorized MARKETING", step.ID)
		}
		if _, err := OrderedParameterBindings(step.Params, "header"); err != nil {
			return fmt.Errorf("step %s header params: %w", step.ID, err)
		}
		if _, err := OrderedParameterBindings(step.Params, "body"); err != nil {
			return fmt.Errorf("step %s body params: %w", step.ID, err)
		}
		for key := range step.Params {
			if !strings.HasPrefix(key, "header.") && !strings.HasPrefix(key, "body.") {
				return fmt.Errorf("step %s has unsupported parameter key %q", step.ID, key)
			}
		}
		for label, condition := range map[string]*ProductCondition{"customer_has_purchased": step.Conditions.CustomerHasPurchased, "customer_has_not_purchased": step.Conditions.CustomerHasNotPurchased} {
			if condition != nil && len(condition.ProductHandles)+len(condition.ProductTags)+len(condition.ProductTitles) == 0 {
				return fmt.Errorf("step %s condition %s needs a product handle, tag, or title", step.ID, label)
			}
		}
	}
	if d.Frequency.Messages < 1 {
		return errors.New("frequency_cap.messages must be positive")
	}
	if _, err := time.ParseDuration(d.Frequency.Window); err != nil {
		return fmt.Errorf("invalid frequency cap window: %w", err)
	}
	if _, err := parseClock(d.QuietHours.Start); err != nil {
		return fmt.Errorf("invalid quiet_hours.start: %w", err)
	}
	if _, err := parseClock(d.QuietHours.End); err != nil {
		return fmt.Errorf("invalid quiet_hours.end: %w", err)
	}
	return nil
}

func ParseWait(value string) (time.Duration, error) {
	dayIndex := strings.Index(value, "d")
	if dayIndex < 0 {
		return time.ParseDuration(value)
	}
	if dayIndex == 0 {
		return 0, errors.New("day duration needs a positive integer")
	}
	days, err := strconv.Atoi(value[:dayIndex])
	if err != nil || days < 0 {
		return 0, errors.New("day duration needs a non-negative integer")
	}
	duration := time.Duration(days) * 24 * time.Hour
	remainder := value[dayIndex+1:]
	if remainder == "" {
		return duration, nil
	}
	rest, err := time.ParseDuration(remainder)
	if err != nil {
		return 0, err
	}
	return duration + rest, nil
}

var allowedParameterSources = map[string]bool{
	"customer.first_name":       true,
	"customer.last_name":        true,
	"order.number":              true,
	"order.first_product_title": true,
}

func OrderedParameterBindings(params map[string]string, component string) ([]string, error) {
	type indexed struct {
		index  int
		source string
	}
	var values []indexed
	prefix := component + "."
	for key, source := range params {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || index < 1 {
			return nil, fmt.Errorf("%s indexes must be positive integers", component)
		}
		if !allowedParameterSources[source] && !strings.HasPrefix(source, "literal:") {
			return nil, fmt.Errorf("unsupported source %q", source)
		}
		if strings.HasPrefix(source, "literal:") && strings.TrimPrefix(source, "literal:") == "" {
			return nil, errors.New("literal parameter cannot be empty")
		}
		values = append(values, indexed{index: index, source: source})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].index < values[j].index })
	result := make([]string, 0, len(values))
	for position, value := range values {
		if value.index != position+1 {
			return nil, fmt.Errorf("%s parameter indexes must be contiguous from 1", component)
		}
		result = append(result, value.source)
	}
	return result, nil
}

func NextAllowedTime(candidate time.Time, timezone, quietStart, quietEnd string) (time.Time, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	startMinutes, err := parseClock(quietStart)
	if err != nil {
		return time.Time{}, err
	}
	endMinutes, err := parseClock(quietEnd)
	if err != nil {
		return time.Time{}, err
	}
	local := candidate.In(location)
	minute := local.Hour()*60 + local.Minute()
	inQuiet := false
	if startMinutes < endMinutes {
		inQuiet = minute >= startMinutes && minute < endMinutes
	} else {
		inQuiet = minute >= startMinutes || minute < endMinutes
	}
	if !inQuiet {
		return candidate, nil
	}
	next := time.Date(local.Year(), local.Month(), local.Day(), endMinutes/60, endMinutes%60, 0, 0, location)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC(), nil
}

func parseClock(value string) (int, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, err
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}
