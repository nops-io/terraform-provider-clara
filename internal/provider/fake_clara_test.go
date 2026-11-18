package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const testAPIKey = "test-key"

// fakeClara implements the create-only Clara onboarding endpoint in memory. It records every
// request so tests can prove the provider sends nothing else.
type fakeClara struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	payers   map[string]string // payer account id -> integration id
	next     int
	requests []string
	bodies   []onboardRequest
	keys     []string

	// verification is what the permission check reports for the next create.
	verification onboardVerification
	status       string
}

func newFakeClara(t *testing.T) *fakeClara {
	t.Helper()
	f := &fakeClara{
		t:            t,
		payers:       map[string]string{},
		status:       "active",
		verification: onboardVerification{State: "verified", RoleAssumption: "ok", FailedChecks: []string{}},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeClara) url() string { return f.server.URL }

// requestLog returns "METHOD path" for every request received, in order.
func (f *fakeClara) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeClara) lastBody() onboardRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		f.t.Fatalf("no create request was received")
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeClara) seed(payer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.payers[payer] = fmt.Sprintf("int-%d", f.next)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": "req-test"}})
}

func (f *fakeClara) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid API key")
		return
	}
	// Create-only: the real endpoint has no other method or path.
	if r.Method != http.MethodPost || r.URL.Path != onboardPath {
		writeError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required for this request")
		return
	}
	f.keys = append(f.keys, key)

	var req onboardRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}
	f.bodies = append(f.bodies, req)

	if _, exists := f.payers[req.PayerAccountID]; exists {
		writeError(w, http.StatusConflict, "already_exists", "An integration for this AWS account already exists")
		return
	}

	f.next++
	id := fmt.Sprintf("int-%d", f.next)
	f.payers[req.PayerAccountID] = id

	name := req.Name
	if name == "" {
		name = "AWS Account " + req.PayerAccountID
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": onboardResult{
		ID:             id,
		Status:         f.status,
		Name:           name,
		PayerAccountID: req.PayerAccountID,
		Verification:   f.verification,
	}})
}
