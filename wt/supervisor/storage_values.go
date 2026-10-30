package main

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func decodeStorageValue(raw jsontext.Value) (any, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty value")
	}
	switch text[0] {
	case 'n':
		return nil, nil
	case 't':
		return true, nil
	case 'f':
		return false, nil
	case '"':
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return value, nil
	case '{':
		var object map[string]string
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf("object parameters must be {\"$b64\": \"...\"}")
		}
		encoded, ok := object["$b64"]
		if !ok {
			return nil, fmt.Errorf("object parameters must be {\"$b64\": \"...\"}")
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		return value, nil
	default:
		if strings.ContainsAny(text, ".eE") {
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, err
			}
			return value, nil
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, err
		}
		return value, nil
	}
}

func decodeStorageParams(raw []jsontext.Value) ([]any, error) {
	if len(raw) > maxStorageParams {
		return nil, fmt.Errorf("too many parameters (max %d)", maxStorageParams)
	}
	params := make([]any, len(raw))
	for i, value := range raw {
		decoded, err := decodeStorageValue(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", i+1, err)
		}
		params[i] = decoded
	}
	return params, nil
}

func encodeStorageValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return map[string]string{"$b64": base64.StdEncoding.EncodeToString(value)}
	case time.Time:
		return value.Format(time.RFC3339Nano)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		return value
	default:
		return value
	}
}

func storageValueSize(value any) int {
	switch value := value.(type) {
	case string:
		return len(value)
	case []byte:
		return base64.StdEncoding.EncodedLen(len(value))
	case int64, float64:
		return 8
	case time.Time:
		return 32
	case nil:
		return 4
	default:
		return 8
	}
}
