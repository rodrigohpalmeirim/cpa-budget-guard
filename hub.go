package main

// Usage-limit hub for T3 Code (and other clients of CLIProxyAPI's usage endpoints).
//
// T3 Code shows a pool's limits by calling CLIProxyAPI's management API: GET auth-files, then POST
// api-call against each provider's usage endpoint. Those need the management key, which can read
// and change everything. These routes answer the same two calls for a budgeted client key
// instead, and api-call only reaches the providers' usage endpoints. A reverse proxy maps the
// client's requests onto them and passes the client's own Authorization header in
// X-Budget-Guard-Key (see README).

import (
	"encoding/json"
	"net/http"
	"strings"
)

const hubKeyHeader = "X-Budget-Guard-Key"

// hubURLs lists, per provider, the only upstream URLs api-call may reach, and their method.
var hubURLs = map[string]map[string]string{
	"claude": {"https://api.anthropic.com/api/oauth/usage": "GET"},
	"codex": {
		"https://chatgpt.com/backend-api/wham/usage":                            "GET",
		"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits":         "GET",
		"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume": "POST",
	},
}

func handleHub(req managementRequest, endpoint string) ([]byte, error) {
	key := strings.TrimSpace(req.Headers.Get(hubKeyHeader))
	key = strings.TrimSpace(strings.TrimPrefix(key, "Bearer "))
	if _, ok := engine.KeyName(key); !ok {
		return hubJSON(http.StatusUnauthorized, map[string]string{"error": "unknown client key"})
	}
	switch {
	case endpoint == "auth-files" && req.Method == http.MethodGet:
		return hubAuthFiles()
	case endpoint == "api-call" && req.Method == http.MethodPost:
		return hubAPICall(req.Body)
	}
	return hubJSON(http.StatusNotFound, map[string]string{"error": "not found"})
}

func hubAuthFiles() ([]byte, error) {
	files, err := listAuthFiles()
	if err != nil {
		return hubJSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	type idToken struct {
		ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`
	}
	type file struct {
		ID        string   `json:"id"`
		AuthIndex string   `json:"auth_index"`
		Provider  string   `json:"provider"`
		Email     string   `json:"email,omitempty"`
		Disabled  bool     `json:"disabled"`
		IDToken   *idToken `json:"id_token,omitempty"`
	}
	out := []file{}
	for _, f := range files {
		provider := f.provider()
		if hubURLs[provider] == nil {
			continue
		}
		item := file{ID: f.ID, AuthIndex: f.AuthIndex, Provider: provider, Email: f.Email, Disabled: f.Disabled}
		if provider == "codex" {
			if c, err := getCredential(f.AuthIndex); err == nil && c.accountID() != "" {
				item.IDToken = &idToken{ChatGPTAccountID: c.accountID()}
			}
		}
		out = append(out, item)
	}
	return hubJSON(http.StatusOK, map[string]any{"files": out})
}

func hubAPICall(body []byte) ([]byte, error) {
	var in struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.AuthIndex == "" {
		return hubJSON(http.StatusBadRequest, map[string]string{"error": "expected auth_index, method, url"})
	}
	files, err := listAuthFiles()
	if err != nil {
		return hubJSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	provider := ""
	for _, f := range files {
		if f.AuthIndex == in.AuthIndex {
			provider = f.provider()
		}
	}
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	if allowed, ok := hubURLs[provider][in.URL]; !ok || allowed != method {
		return hubJSON(http.StatusForbidden, map[string]string{"error": "only the providers' usage endpoints are reachable"})
	}
	cred, err := getCredential(in.AuthIndex)
	if err != nil {
		return hubJSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	headers := http.Header{}
	for k, v := range in.Header {
		headers.Set(k, strings.ReplaceAll(v, "$TOKEN$", cred.AccessToken))
	}
	var resp hostHTTPResponse
	if err := callHost("host.http.do", hostHTTPRequest{Method: method, URL: in.URL, Headers: headers, Body: []byte(in.Data)}, &resp); err != nil {
		return hubJSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return hubJSON(http.StatusOK, map[string]any{"status_code": resp.StatusCode, "body": string(resp.Body)})
}

func hubJSON(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: status,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}
