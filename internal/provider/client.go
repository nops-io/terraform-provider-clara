package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 4 << 20
	maxRetries       = 3
	maxRetryAfter    = 30 * time.Second
)

// apiClient talks to the Clara public API. The API wraps successful responses as
// {"data": ...} and failures as {"error": {"code", "message", "request_id"}}.
type apiClient struct {
	baseURL      string
	apiKey       string
	userAgent    string
	http         *http.Client
	retryBackoff time.Duration
	pollInterval time.Duration
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

// flexString decodes a JSON string or number into a string. Clara client ids are
// numbers internally and may be serialized either way.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

type contextResponse struct {
	ClientID       flexString `json:"client_id"`
	OrganizationID flexString `json:"organization_id"`
}

// awsIntegrationRequest is the body for create and update. It never comes back in a
// response: external_id is write-only on the server side.
type awsIntegrationRequest struct {
	PayerAccountID string `json:"payer_account_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Method         string `json:"method,omitempty"`
	PermissionMode string `json:"permission_mode"`
	RoleArn        string `json:"role_arn"`
	ExternalID     string `json:"external_id"`
	CurBucketName  string `json:"cur_bucket_name"`
	CurReportName  string `json:"cur_report_name,omitempty"`
	CurS3Prefix    string `json:"cur_s3_prefix,omitempty"`
	Cur2Enabled    bool   `json:"cur2_enabled"`
	Cur2ExportName string `json:"cur2_export_name,omitempty"`
	Cur2S3Prefix   string `json:"cur2_s3_prefix,omitempty"`
}

type awsIntegration struct {
	ID             flexString `json:"id"`
	Status         string     `json:"status"`
	PayerAccountID string     `json:"payer_account_id"`
	Name           string     `json:"name"`
	PermissionMode string     `json:"permission_mode"`
	RoleArn        string     `json:"role_arn"`
	CurBucketName  string     `json:"cur_bucket_name"`
	CurReportName  string     `json:"cur_report_name"`
	CurS3Prefix    string     `json:"cur_s3_prefix"`
	Cur2Enabled    bool       `json:"cur2_enabled"`
	Cur2ExportName string     `json:"cur2_export_name"`
	Cur2S3Prefix   string     `json:"cur2_s3_prefix"`
}

type verifyResponse struct {
	Status string `json:"status"`
}

func (c *apiClient) getContext(ctx context.Context) (*contextResponse, error) {
	var out contextResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/aws/context", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// findIntegration returns the integration for a payer account, or nil.
func (c *apiClient) findIntegration(ctx context.Context, payerAccountID string) (*awsIntegration, error) {
	q := url.Values{"payerAccountId": []string{payerAccountID}}
	var out []awsIntegration
	if err := c.do(ctx, http.MethodGet, "/api/v1/aws/integrations", q, nil, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].PayerAccountID == payerAccountID {
			return &out[i], nil
		}
	}
	return nil, nil
}

func (c *apiClient) createIntegration(ctx context.Context, in awsIntegrationRequest) (*awsIntegration, error) {
	var out awsIntegration
	if err := c.do(ctx, http.MethodPost, "/api/v1/aws/integrations", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *apiClient) getIntegration(ctx context.Context, id string) (*awsIntegration, error) {
	var out awsIntegration
	if err := c.do(ctx, http.MethodGet, "/api/v1/aws/integrations/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *apiClient) updateIntegration(ctx context.Context, id string, in awsIntegrationRequest) (*awsIntegration, error) {
	// The payer account and method are fixed once an integration exists.
	in.PayerAccountID = ""
	in.Method = ""
	var out awsIntegration
	if err := c.do(ctx, http.MethodPatch, "/api/v1/aws/integrations/"+url.PathEscape(id), nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *apiClient) verifyIntegration(ctx context.Context, id string) (string, error) {
	var out verifyResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/aws/integrations/"+url.PathEscape(id)+"/verify-sa", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func (c *apiClient) disconnectIntegration(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/aws/integrations/"+url.PathEscape(id)+"/disconnect", nil, nil, nil)
}

// do sends one request, retrying on 429 and 503. out may be nil.
func (c *apiClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	target := strings.TrimRight(c.baseURL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

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
		if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
			return nil
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
