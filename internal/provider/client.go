package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 4 << 20
	maxRetries       = 3
	maxRetryAfter    = 30 * time.Second

	onboardPath = "/v1/aws/onboarding"
)

// apiClient talks to the Clara public API. The API wraps successful responses as
// {"data": ...} and failures as {"error": {"code", "message", "request_id"}}.
//
// It exposes exactly one operation, creating an integration. The API key this provider is
// meant for can do nothing else, so there is no read, update or delete call to add.
type apiClient struct {
	baseURL      string
	apiKey       string
	userAgent    string
	http         *http.Client
	retryBackoff time.Duration
}

// apiError is a non-2xx response from Clara.
type apiError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("Clara API returned %d", e.Status)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RequestID != "" {
		msg += " [request " + e.RequestID + "]"
	}
	return msg
}

func statusOf(err error) int {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

type onboardCur struct {
	BucketName string `json:"bucketName"`
	ReportName string `json:"reportName,omitempty"`
	S3Prefix   string `json:"s3Prefix,omitempty"`
}

type onboardCur2 struct {
	Enabled    bool   `json:"enabled"`
	ExportName string `json:"exportName,omitempty"`
	S3Prefix   string `json:"s3Prefix,omitempty"`
}

// onboardRequest is the body of POST /v1/aws/onboarding. The ExternalId is write-only: Clara
// never returns it.
type onboardRequest struct {
	PayerAccountID string       `json:"payerAccountId"`
	Name           string       `json:"name,omitempty"`
	RoleArn        string       `json:"roleArn"`
	ExternalID     string       `json:"externalId"`
	PermissionMode string       `json:"permissionMode"`
	Cur            onboardCur   `json:"cur"`
	Cur2           *onboardCur2 `json:"cur2,omitempty"`
}

type onboardVerification struct {
	// State is verified, partial, failed or unavailable.
	State          string   `json:"state"`
	RoleAssumption string   `json:"roleAssumption"`
	FailedChecks   []string `json:"failedChecks"`
	Message        *string  `json:"message"`
}

type onboardResult struct {
	ID             string              `json:"id"`
	Status         string              `json:"status"`
	Name           string              `json:"name"`
	PayerAccountID string              `json:"payerAccountId"`
	Verification   onboardVerification `json:"verification"`
}

// onboardAccount registers one payer account. idempotencyKey makes a retry after a lost
// response return the first result instead of registering twice.
func (c *apiClient) onboardAccount(ctx context.Context, in onboardRequest, idempotencyKey string) (*onboardResult, error) {
	var out onboardResult
	if err := c.post(ctx, onboardPath, idempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post sends one POST, retrying on 429 and 503 with the same Idempotency-Key.
func (c *apiClient) post(ctx context.Context, path, idempotencyKey string, body, out any) error {
	target := strings.TrimRight(c.baseURL, "/") + path

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Idempotency-Key", idempotencyKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("calling Clara: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("reading Clara response: %w", readErr)
		}

		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) && attempt < maxRetries {
			wait := c.retryBackoff * time.Duration(attempt+1)
			if secs, convErr := strconv.Atoi(resp.Header.Get("Retry-After")); convErr == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if resp.StatusCode >= 400 {
			return parseAPIError(resp.StatusCode, raw)
		}

		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("decoding Clara response: %w", err)
		}
		if len(envelope.Data) == 0 {
			return errors.New("Clara response has no data field")
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decoding Clara response data: %w", err)
		}
		return nil
	}
}

func parseAPIError(status int, raw []byte) error {
	e := &apiError{Status: status}
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
		e.RequestID = envelope.Error.RequestID
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
