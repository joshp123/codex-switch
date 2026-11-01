package main

import (
	"crypto/subtle"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/t3pb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// t3UsageOrRelay implements the read-only subset of T3's CLIProxy source contract.
func t3UsageOrRelay(capability, directory string, monitor *accounts.Monitor, codexOrigin, claudeOrigin *url.URL, transport http.RoundTripper, next http.Handler) http.Handler {
	prefix := "/v0/management/"
	client := &http.Client{Timeout: 12 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	write := func(w http.ResponseWriter, message proto.Message) {
		data, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(message)
		if err != nil {
			http.Error(w, "cannot encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+capability)) != 1 {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == prefix+"auth-files":
			pool, err := accounts.ReadPool(directory)
			if err != nil {
				http.Error(w, "cannot read accounts", 500)
				return
			}
			result := &t3pb.AuthFiles{}
			for _, account := range pool {
				provider := "codex"
				var identity *t3pb.AuthIdentity
				if account.Provider == accounts.ProviderClaude {
					provider = "claude"
				} else {
					identity = &t3pb.AuthIdentity{ChatgptAccountId: account.AccountID}
				}
				result.Files = append(result.Files, &t3pb.AuthFile{Id: account.Name, AuthIndex: account.Name, Provider: provider, Email: account.Email, IdToken: identity})
			}
			write(w, result)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"api-call":
			data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
			if err != nil || len(data) > 65536 {
				http.Error(w, "invalid request", 400)
				return
			}
			call := &t3pb.ApiCall{}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, call); err != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			var target *url.URL
			var provider accounts.Provider
			switch call.Url {
			case "https://chatgpt.com/backend-api/wham/usage":
				target = codexOrigin.ResolveReference(&url.URL{Path: "/backend-api/wham/usage"})
			case "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits":
				target = codexOrigin.ResolveReference(&url.URL{Path: "/backend-api/wham/rate-limit-reset-credits"})
			case "https://api.anthropic.com/api/oauth/usage":
				target = claudeOrigin.ResolveReference(&url.URL{Path: "/api/oauth/usage"})
				provider = accounts.ProviderClaude
			}
			if call.Method != http.MethodGet || target == nil {
				write(w, &t3pb.ApiResponse{StatusCode: 403, Body: "Only usage reads are supported; reset credits cannot be redeemed here."})
				return
			}
			facts, err := monitor.ReadAccountRoutingFacts()
			if err != nil {
				http.Error(w, "cannot read accounts", 500)
				return
			}
			var credential accounts.Credentials
			var selected accounts.AccountRoutingFact
			for _, fact := range facts {
				if string(fact.AccountName) == call.AuthIndex && fact.Provider == provider {
					credential = fact.Credentials
					selected = fact
					break
				}
			}
			if credential.AccessToken == "" {
				write(w, &t3pb.ApiResponse{StatusCode: 404, Body: "Account credentials unavailable."})
				return
			}
			if provider == accounts.ProviderClaude {
				if selected.AllowanceSample.ObservedAt.IsZero() {
					write(w, &t3pb.ApiResponse{StatusCode: 503, Body: "Claude usage is unavailable."})
					return
				}
				usage := &t3pb.ClaudeUsage{}
				for _, window := range selected.AllowanceSample.Windows {
					if window.ResetsAt.IsZero() {
						continue
					}
					reset := window.ResetsAt.UTC().Format(time.RFC3339)
					if window.ModelName != "" {
						usage.Limits = append(usage.Limits, &t3pb.ClaudeLimit{Kind: "weekly_scoped", Percent: window.UsedPercent, ResetsAt: reset, Scope: &t3pb.ClaudeScope{Model: &t3pb.ClaudeModel{DisplayName: window.ModelName}}})
					} else if window.DurationSeconds == 5*3600 {
						usage.FiveHour = &t3pb.ClaudeWindow{Utilization: window.UsedPercent, ResetsAt: reset}
					} else if window.DurationSeconds == 7*86400 {
						usage.SevenDay = &t3pb.ClaudeWindow{Utilization: window.UsedPercent, ResetsAt: reset}
					}
				}
				body, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(usage)
				if err != nil {
					http.Error(w, "cannot encode usage", 500)
					return
				}
				write(w, &t3pb.ApiResponse{StatusCode: 200, Body: string(body)})
				return
			}
			upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
			if err != nil {
				http.Error(w, "cannot create request", 500)
				return
			}
			upstream.Header.Set("Authorization", "Bearer "+credential.AccessToken)
			if provider == accounts.ProviderClaude {
				upstream.Header.Set("anthropic-beta", "oauth-2025-04-20")
				upstream.Header.Set("anthropic-version", "2023-06-01")
				upstream.Header.Set("User-Agent", "claude-code/2.1.273")
			} else {
				upstream.Header.Set("ChatGPT-Account-ID", credential.AccountID)
				upstream.Header.Set("OpenAI-Beta", "codex-1")
				upstream.Header.Set("Originator", "Codex Desktop")
			}
			response, err := client.Do(upstream)
			if err != nil {
				http.Error(w, "provider request failed", 502)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
			if err != nil || len(body) > 1024*1024 {
				http.Error(w, "provider response failed", 502)
				return
			}
			write(w, &t3pb.ApiResponse{StatusCode: int32(response.StatusCode), Body: string(body)})
		default:
			http.Error(w, "unsupported management operation", http.StatusNotFound)
		}
	})
}
