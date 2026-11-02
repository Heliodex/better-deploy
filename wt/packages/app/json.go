package app

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"unicode"
)

// normalizeFieldName converts a generated Go field name to the JSON spelling for the common cases.
func normalizeFieldName(name string) string {
	if name == "" {
		return name
	}

	if name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}

	return strings.ToLower(name[:1]) + name[1:]
}

// lowerFieldName lowercases the first character of a json/v2 "field X" message.
func lowerFieldName(message string) string {
	const prefix = "field "
	if !strings.HasPrefix(message, prefix) {
		return message
	}

	rest := message[len(prefix):]
	if rest == "" {
		return message
	}

	end := strings.IndexFunc(rest, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if end < 0 {
		end = len(rest)
	}

	return prefix + normalizeFieldName(rest[:end]) + rest[end:]
}

// jsonErrorMessage trims the encoding/json/v2 wrapper from a generated validation error, leaving the field and the rule that failed.
func jsonErrorMessage(err error, field string) string {
	message := err.Error()
	if index := strings.Index(message, ": field "); index >= 0 {
		return lowerFieldName(message[index+2:])
	}

	if index := strings.Index(message, ": invalid value"); index >= 0 {
		return "field " + field + ": " + message[index+2:]
	}

	if _, after, ok := strings.CutLast(message, ": "); ok {
		return after
	}

	return message
}

// jsonErrorField extracts the field named at the start of a formatted validation error, falling back to the caller's field.
func jsonErrorField(message, fallback string) string {
	const marker = "field "
	if !strings.HasPrefix(message, marker) {
		return fallback
	}

	rest := message[len(marker):]

	end := strings.IndexFunc(rest, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if end < 0 {
		end = len(rest)
	}

	name := rest[:end]
	if name == "" {
		return fallback
	}

	return normalizeFieldName(name)
}

// ReadJSON decodes a request body into out, writing a 400 with a readable field error when it cannot. field names the value a bare invalid-value error refers to.
func ReadJSON(w http.ResponseWriter, r *http.Request, limit int64, field string, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.UnmarshalRead(r.Body, out); err != nil {
		message := jsonErrorMessage(err, field)
		WriteErrorField(w, http.StatusBadRequest, "invalid", jsonErrorField(message, field), message)

		return false
	}

	return true
}
