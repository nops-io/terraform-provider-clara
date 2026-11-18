package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testAPIKey = "test-key"

// fakeClara implements the proposed Clara AWS onboarding API in memory.
type fakeClara struct {
	t      *testing.T
	server *httptest.Server

	mu    sync.Mutex
	items map[string]*awsIntegration
	next  int
	log   []string

	// verifyStatus is the status verify-sa produces. If it is "validating", the
	// integration turns active after validatingPolls GET requests.
	verifyStatus    string
	validatingPolls int
	pollsLeft       map[string]int
}

func newFakeClara(t *testing.T) *fakeClara {
	t.Helper()
	f := &fakeClara{
		t:            t,
		items:        map[string]*awsIntegration{},
		verifyStatus: "active",
		pollsLeft:    map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeClara) url() string { return f.server.URL }

func (f *fakeClara) saw(entry string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		if l == entry {
			return true
		}
	}
	return false
}

func (f *fakeClara) status(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		return it.Status
	}
	return ""
}

func (f *fakeClara) seed(payer string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("int-%d", f.next)
	f.items[id] = &awsIntegration{
		ID: flexString(id), Status: "validating", PayerAccountID: payer, Name: "Existing",
		PermissionMode: "minimal", RoleArn: "arn:aws:iam::" + payer + ":role/old", CurBucketName: "old-bucket",
		CurReportName: "old_report", CurS3Prefix: "old-prefix",
	}
	return id
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
	f.log = append(f.log, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "bad key")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/aws")
	switch {
	case path == "/context" && r.Method == http.MethodGet:
		// The client id is a number in Clara; the provider must accept it.
		writeJSON(w, 200, map[string]any{"data": map[string]any{"client_id": 42, "organization_id": "org_abc"}})

	case path == "/integrations" && r.Method == http.MethodGet:
		payer := r.URL.Query().Get("payerAccountId")
		out := []*awsIntegration{}
		for _, it := range f.items {
			if it.PayerAccountID == payer && it.Status != "disconnected" {
				out = append(out, it)
			}
		}
		writeJSON(w, 200, map[string]any{"data": out})

	case path == "/integrations" && r.Method == http.MethodPost:
		var req awsIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
		if req.Method != "terraform" {
			writeError(w, 400, "invalid_request", "method must be terraform")
			return
		}
		for _, it := range f.items {
			if it.PayerAccountID == req.PayerAccountID && it.Status != "disconnected" {
				writeError(w, http.StatusConflict, "integration_exists", "already onboarded")
				return
			}
		}
		f.next++
		id := fmt.Sprintf("int-%d", f.next)
		it := &awsIntegration{ID: flexString(id), Status: "validating", PayerAccountID: req.PayerAccountID}
		f.apply(it, req)
		f.items[id] = it
		writeJSON(w, 201, map[string]any{"data": it})

	case strings.HasPrefix(path, "/integrations/"):
		rest := strings.TrimPrefix(path, "/integrations/")
		id, action, _ := strings.Cut(rest, "/")
		it, ok := f.items[id]
		if !ok {
			writeError(w, 404, "not_found", "no such integration")
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			if f.pollsLeft[id] > 0 {
				f.pollsLeft[id]--
				if f.pollsLeft[id] == 0 {
					it.Status = "active"
				}
			}
			writeJSON(w, 200, map[string]any{"data": it})
		case action == "" && r.Method == http.MethodPatch:
			var req awsIntegrationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.apply(it, req)
			writeJSON(w, 200, map[string]any{"data": it})
		case action == "verify-sa" && r.Method == http.MethodPost:
			it.Status = f.verifyStatus
			if f.verifyStatus == "validating" {
				f.pollsLeft[id] = f.validatingPolls
			}
			writeJSON(w, 200, map[string]any{"data": map[string]any{"status": it.Status}})
		case action == "disconnect" && r.Method == http.MethodPost:
			it.Status = "disconnected"
			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, 405, "method_not_allowed", "no route")
		}

	default:
		writeError(w, 404, "not_found", "no route")
	}
}

// apply copies request fields, filling the defaults Clara's wizard suggests.
func (f *fakeClara) apply(it *awsIntegration, req awsIntegrationRequest) {
	it.PermissionMode = req.PermissionMode
	it.RoleArn = req.RoleArn
	it.CurBucketName = req.CurBucketName
	it.Name = firstNonEmpty(req.Name, it.Name, "AWS Account "+it.PayerAccountID)
	it.CurReportName = firstNonEmpty(req.CurReportName, it.CurReportName, "nops_cur_42_hourly")
	it.CurS3Prefix = firstNonEmpty(req.CurS3Prefix, it.CurS3Prefix, "nops-cur")
	it.Cur2Enabled = req.Cur2Enabled
	if req.Cur2Enabled {
		it.Cur2ExportName = req.Cur2ExportName
		it.Cur2S3Prefix = firstNonEmpty(req.Cur2S3Prefix, "nops-cur2")
	} else {
		it.Cur2ExportName = ""
		it.Cur2S3Prefix = ""
	}
	it.Status = "validating"
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
