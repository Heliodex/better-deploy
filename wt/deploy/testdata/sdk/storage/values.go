package storage

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

func encodeParam(arg any) any {
	switch value := arg.(type) {
	case []byte:
		return map[string]string{"$b64": base64.StdEncoding.EncodeToString(value)}
	case time.Time:
		return value.Format(time.RFC3339Nano)
	case bool:
		if value {
			return int64(1)
		}
		return int64(0)
	case int:
		return int64(value)
	case int8:
		return int64(value)
	case int16:
		return int64(value)
	case int32:
		return int64(value)
	case uint:
		return uint64(value)
	case uint8:
		return int64(value)
	case uint16:
		return int64(value)
	case uint32:
		return int64(value)
	default:
		return arg
	}
}

func encodeParams(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	params := make([]any, len(args))
	for i, arg := range args {
		params[i] = encodeParam(arg)
	}
	return params
}

// decodeBlobValue decodes the {"$b64": "..."} form used for byte parameters.
func decodeBlobValue(raw jsontext.Value) ([]byte, error) {
	var object map[string]string
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("blob must be {\"$b64\": \"...\"}")
	}
	encoded, ok := object["$b64"]
	if !ok {
		return nil, fmt.Errorf("blob must be {\"$b64\": \"...\"}")
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid base64: %w", err)
	}
	return value, nil
}

// decodeNumberValue parses a JSON number, keeping integers exact unless the text carries a fraction or exponent.
func decodeNumberValue(text string) (any, error) {
	if strings.ContainsAny(text, ".eE") {
		return strconv.ParseFloat(text, 64)
	}
	return strconv.ParseInt(text, 10, 64)
}

func decodeValue(raw jsontext.Value) (any, error) {
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
		return decodeBlobValue(raw)
	default:
		return decodeNumberValue(text)
	}
}

func int64Value(value any) (int64, error) {
	switch value := value.(type) {
	case int64:
		return value, nil
	case float64:
		if value != math.Trunc(value) || value < math.MinInt64 || value > math.MaxInt64 {
			return 0, fmt.Errorf("cannot convert %v to int64", value)
		}
		return int64(value), nil
	case bool:
		if value {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseInt(value, 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", value)
	}
}

func float64Value(value any) (float64, error) {
	switch value := value.(type) {
	case float64:
		return value, nil
	case int64:
		return float64(value), nil
	case string:
		return strconv.ParseFloat(value, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", value)
	}
}

func boolValue(value any) (bool, error) {
	switch value := value.(type) {
	case bool:
		return value, nil
	case int64:
		switch value {
		case 0:
			return false, nil
		case 1:
			return true, nil
		default:
			return false, fmt.Errorf("cannot convert %d to bool", value)
		}
	case string:
		return strconv.ParseBool(value)
	default:
		return false, fmt.Errorf("cannot convert %T to bool", value)
	}
}

func scanValue(dest, value any) error {
	if scanner, ok := dest.(interface{ Scan(any) error }); ok {
		return scanner.Scan(value)
	}
	switch dest := dest.(type) {
	case *any:
		*dest = value
	case *string:
		switch value := value.(type) {
		case string:
			*dest = value
		case []byte:
			*dest = string(value)
		case int64:
			*dest = strconv.FormatInt(value, 10)
		case float64:
			*dest = strconv.FormatFloat(value, 'g', -1, 64)
		default:
			return fmt.Errorf("cannot scan %T into *string", value)
		}
	case *[]byte:
		switch value := value.(type) {
		case []byte:
			*dest = value
		case string:
			*dest = []byte(value)
		default:
			return fmt.Errorf("cannot scan %T into *[]byte", value)
		}
	case *int64:
		value, err := int64Value(value)
		if err != nil {
			return err
		}
		*dest = value
	case *int:
		value, err := int64Value(value)
		if err != nil {
			return err
		}
		if int64(int(value)) != value {
			return fmt.Errorf("integer %d overflows int", value)
		}
		*dest = int(value)
	case *float64:
		value, err := float64Value(value)
		if err != nil {
			return err
		}
		*dest = value
	case *bool:
		value, err := boolValue(value)
		if err != nil {
			return err
		}
		*dest = value
	case *time.Time:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("cannot scan %T into *time.Time", value)
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return err
		}
		*dest = parsed
	default:
		return fmt.Errorf("cannot scan into %T", dest)
	}
	return nil
}
