package app

import (
	"encoding/json/v2"
	"net/http"
	"strings"
)

func lowerFieldName(message string) string {
	const prefix = "field "
	if !strings.HasPrefix(message, prefix) {
		return message
	}
	rest := message[len(prefix):]
	if rest == "" {
		return message
	}
	return prefix + strings.ToLower(rest[:1]) + rest[1:]
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

// ReadJSON decodes a request body into out, writing a 400 with a readable field error when it cannot. field names the value a bare invalid-value error refers to.
func ReadJSON(w http.ResponseWriter, r *http.Request, limit int64, field string, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.UnmarshalRead(r.Body, out); err != nil {
		WriteError(w, http.StatusBadRequest, jsonErrorMessage(err, field))
		return false
	}
	return true
}
