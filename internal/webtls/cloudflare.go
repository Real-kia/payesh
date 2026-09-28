package webtls

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var cloudflareAPI = "https://api.cloudflare.com/client/v4"

// cloudflare publishes DNS-01 records with a token scoped to Zone:DNS:Edit.
type cloudflare struct {
	token  string
	client *http.Client
}

type cloudflareResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c cloudflare) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	request, err := http.NewRequestWithContext(ctx, method, cloudflareAPI+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var decoded cloudflareResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("unexpected response (%s)", response.Status)
	}
	if !decoded.Success {
		messages := make([]string, 0, len(decoded.Errors))
		for _, e := range decoded.Errors {
			messages = append(messages, e.Message)
		}
		if len(messages) == 0 {
			messages = append(messages, response.Status)
		}
		return nil, errors.New(strings.Join(messages, "; "))
	}
	return decoded.Result, nil
}

// zoneID finds the Cloudflare zone that owns name by trying each parent.
func (c cloudflare) zoneID(ctx context.Context, name string) (string, error) {
	labels := strings.Split(name, ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		result, err := c.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(candidate), nil)
		if err != nil {
			return "", err
		}
		var zones []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(result, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone found for %s; is the domain on this Cloudflare account and does the token have Zone:DNS:Edit?", name)
}

// publishTXT creates the record and returns a cleanup that deletes it.
func (c cloudflare) publishTXT(ctx context.Context, name, value string) (func(), error) {
	if c.token == "" {
		return nil, errors.New("no Cloudflare API token is configured")
	}
	zone, err := c.zoneID(ctx, strings.TrimPrefix(name, "_acme-challenge."))
	if err != nil {
		return nil, err
	}
	result, err := c.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/dns_records", map[string]any{"type": "TXT", "name": name, "content": value, "ttl": 60})
	if err != nil {
		return nil, err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &record); err != nil {
		return nil, err
	}
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.call(cleanupCtx, http.MethodDelete, "/zones/"+url.PathEscape(zone)+"/dns_records/"+url.PathEscape(record.ID), nil)
	}, nil
}
