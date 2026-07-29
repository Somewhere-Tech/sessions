package state

import (
	"fmt"
	"strings"
)

const (
	MaxSessionTags     = 32
	MaxSessionTagKey   = 64
	MaxSessionTagValue = 256
)

// NormalizeTags validates and canonicalizes user-defined session dimensions.
// Keys are case-insensitive and stored lowercase so product=Sessions and
// Product=Sessions cannot silently become two different dimensions.
func NormalizeTags(input map[string]string) (map[string]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	if len(input) > MaxSessionTags {
		return nil, fmt.Errorf("sessions support at most %d tags", MaxSessionTags)
	}
	normalized := make(map[string]string, len(input))
	for rawKey, rawValue := range input {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value := strings.TrimSpace(rawValue)
		if key == "" {
			return nil, fmt.Errorf("tag key cannot be empty; use key=value (for example product=Sessions)")
		}
		if len(key) > MaxSessionTagKey {
			return nil, fmt.Errorf("tag key %q is longer than %d characters", key, MaxSessionTagKey)
		}
		if !validTagKey(key) {
			return nil, fmt.Errorf("invalid tag key %q; use letters, numbers, '.', '_', or '-'", key)
		}
		if value == "" {
			return nil, fmt.Errorf("tag %q needs a value; remove the key instead of assigning an empty value", key)
		}
		if len(value) > MaxSessionTagValue {
			return nil, fmt.Errorf("tag %q value is longer than %d characters", key, MaxSessionTagValue)
		}
		if _, duplicate := normalized[key]; duplicate {
			return nil, fmt.Errorf("tag key %q was supplied more than once with different casing", key)
		}
		normalized[key] = value
	}
	return normalized, nil
}

func validTagKey(value string) bool {
	for index, character := range value {
		letter := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if index == 0 && !letter && !digit {
			return false
		}
		if !letter && !digit && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func CloneTags(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
