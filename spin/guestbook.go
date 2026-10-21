package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spinframework/spin-go-sdk/v3/kv"
)

const (
	guestbookKey  = "guestbook/messages"
	maxNameLength = 40
	maxBodyLength = 280
	maxMessages   = 50
)

// Message is a single guestbook entry stored in the default Spin key-value store and sent to the frontend as an Inertia prop.
type Message struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

func loadMessages() ([]Message, error) {
	store, err := kv.OpenDefault()
	if err != nil {
		return nil, err
	}

	data, err := store.Get(guestbookKey)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []Message{}, nil
	}

	var messages []Message
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}

func saveMessages(messages []Message) error {
	data, err := json.Marshal(messages)
	if err != nil {
		return err
	}

	store, err := kv.OpenDefault()
	if err != nil {
		return err
	}

	return store.Set(guestbookKey, data)
}

func addMessage(name, body string) error {
	messages, err := loadMessages()
	if err != nil {
		return err
	}

	id, err := newMessageID()
	if err != nil {
		return err
	}

	messages = append(messages, Message{
		ID:        id,
		Name:      name,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	})

	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
	}

	return saveMessages(messages)
}

func newMessageID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

type messagePayload struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// readMessagePayload handles both an Inertia XHR request (JSON) and a plain HTML form submission (application/x-www-form-urlencoded).
func readMessagePayload(r *http.Request) (messagePayload, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var payload messagePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return messagePayload{}, err
		}
		return payload, nil
	}

	if err := r.ParseForm(); err != nil {
		return messagePayload{}, err
	}

	return messagePayload{
		Name: r.PostFormValue("name"),
		Body: r.PostFormValue("body"),
	}, nil
}

// validateMessage returns a field → error message map for invalid input.
func validateMessage(name, body string) map[string]string {
	errs := map[string]string{}

	switch {
	case name == "":
		errs["name"] = "Please enter your name."
	case utf8.RuneCountInString(name) > maxNameLength:
		errs["name"] = fmt.Sprintf("Name must be %d characters or fewer.", maxNameLength)
	}

	switch {
	case body == "":
		errs["body"] = "Please write a message."
	case utf8.RuneCountInString(body) > maxBodyLength:
		errs["body"] = fmt.Sprintf("Message must be %d characters or fewer.", maxBodyLength)
	}

	return errs
}
