// Package storage is a small client for the supervisor's per-app key-value service. It reads WT_STORAGE_URL and WT_STORAGE_TOKEN from the environment.
package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type Client struct {
	baseURL, token string
	http           *http.Client
}

// New creates a client using the supplied transport. A wasi:http transport is used in components; tests can pass a normal http.Client.

func New(httpClient *http.Client) (*Client, error) {
	baseURL := strings.TrimSuffix(os.Getenv("WT_STORAGE_URL"), "/")
	token := os.Getenv("WT_STORAGE_TOKEN")
	if baseURL == "" || token == "" {
		return nil, fmt.Errorf("WT_STORAGE_URL and WT_STORAGE_TOKEN must be set")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, http: httpClient}, nil
}

func (c *Client) request(method, key, prefix string) (*http.Request, error) {
	target := c.baseURL + "/v1/kv"
	if key != "" {
		target += "/" + url.PathEscape(key)
	}
	if prefix != "" {
		target += "?prefix=" + url.QueryEscape(prefix)
	}
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return fmt.Errorf("storage: %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func (c *Client) Get(key string) ([]byte, bool, error) {
	req, err := c.request(http.MethodGet, key, "")
	if err != nil {
		return nil, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, responseError(resp)
	}
	value, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (c *Client) Put(key string, value []byte) error {
	req, err := c.request(http.MethodPut, key, "")
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(value))
	req.ContentLength = int64(len(value))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	return nil
}

func (c *Client) Delete(key string) error {
	req, err := c.request(http.MethodDelete, key, "")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return responseError(resp)
	}
	return nil
}

func (c *Client) List(prefix string) ([]string, error) {
	req, err := c.request(http.MethodGet, "", prefix)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var out struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}
