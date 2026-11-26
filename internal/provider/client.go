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
	// While the first attempt with a key is still running, Clara answers 409 idempotency_conflict.
	// Clara finishes a request within 60 seconds, so waiting this many times covers it.
	maxInFlightWaits = 12

	onboardPath = "/v1/aws/onboarding"

	idempotencyConflictCode = "idempotency_conflict"
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

// onboardAccount registers one payer account. Clara stores the response under idempotencyKey,
// so a retry after a lost response returns the first result instead of registering twice.
func (c *apiClient) onboardAccount(ctx context.Context, in onboardRequest, idempotencyKey string) (*onboardResult, error) {
	var out onboardResult
	if err := c.post(ctx, onboardPath, idempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post sends one POST and retries with the same Idempotency-Key when a retry is safe and
// useful: the response was lost (network error, 502, 504) or Clara is busy (429, 503). While
// the first attempt with this key is still running (409 idempotency_conflict) it waits for it
// to finish, up to maxInFlightWaits times. Clara replays a
// completed response for a repeated key, so a retry never registers twice.
func (c *apiClient) post(ctx context.Context, path, idempotencyKey string, body, out any) error {
	target := strings.TrimRight(c.baseURL, "/") + path

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}

	inFlightWaits := 0
	for attempt := 0; ; attempt++ {
		status, raw, header, err := c.send(ctx, target, idempotencyKey, payload)
		if err != nil {
			if ctx.Err() != nil || attempt >= maxRetries {
				return err
			}
			if waitErr := c.wait(ctx, attempt, nil); waitErr != nil {
				return waitErr
			}
			continue
		}

		if status >= 400 {
			apiErr := parseAPIError(status, raw)
			if isInFlight(status, apiErr) && inFlightWaits < maxInFlightWaits {
				// Not a failure of this attempt, so it does not use up a retry.
				inFlightWaits++
				attempt--
				if waitErr := c.sleep(ctx, 5*c.retryBackoff); waitErr != nil {
					return waitErr
				}
				continue
			}
			if attempt < maxRetries && isRetryable(status, apiErr) {
				if waitErr := c.wait(ctx, attempt, header); waitErr != nil {
					return waitErr
				}
				continue
			}
			return apiErr
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

// send makes one attempt. A non-nil error means no usable response arrived.
func (c *apiClient) send(ctx context.Context, target, idempotencyKey string, payload []byte) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("calling Clara: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return 0, nil, nil, fmt.Errorf("reading Clara response: %w", readErr)
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// isRetryable reports whether repeating the request could succeed. A daily quota does not
// reset within the retry window, so quota_exceeded fails at once.
func isRetryable(status int, err error) bool {
	code := ""
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		code = apiErr.Code
	}
	switch status {
	case http.StatusTooManyRequests:
		return code != "quota_exceeded"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isInFlight reports Clara's answer for a key whose first request has not finished yet.
func isInFlight(status int, err error) bool {
	var apiErr *apiError
	return status == http.StatusConflict && errors.As(err, &apiErr) && apiErr.Code == idempotencyConflictCode
}

// wait sleeps before the next attempt: linear backoff, or Retry-After in seconds when the
// response sent one, capped at maxRetryAfter.
func (c *apiClient) wait(ctx context.Context, attempt int, header http.Header) error {
	wait := c.retryBackoff * time.Duration(attempt+1)
	if header != nil {
		if secs, convErr := strconv.Atoi(header.Get("Retry-After")); convErr == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	return c.sleep(ctx, wait)
}

func (c *apiClient) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
