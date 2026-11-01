package t3usage

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/t3pb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const prefix = "/v0/management/"
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
const codexCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

var jsonOptions = protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}

// Handler implements the read-only subset of T3's CLIProxy usage contract.
func Handler(capability, directory string, monitor *accounts.Monitor, next http.Handler) http.Handler {
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
			if call.Method != http.MethodGet || (call.Url != codexUsageURL && call.Url != codexCreditsURL && call.Url != claudeUsageURL) {
				write(w, &t3pb.ApiResponse{StatusCode: 403, Body: "Only usage reads are supported; reset credits cannot be redeemed here."})
				return
			}
			facts, err := monitor.ReadAccountRoutingFacts()
			if err != nil {
				http.Error(w, "cannot read accounts", 500)
				return
			}
			for _, fact := range facts {
				if string(fact.AccountName) != call.AuthIndex || (fact.Provider == accounts.ProviderClaude) != (call.Url == claudeUsageURL) {
					continue
				}
				var message proto.Message
				switch call.Url {
				case claudeUsageURL:
					if !fact.AllowanceSample.ObservedAt.IsZero() {
						message = claudeUsage(fact.AllowanceSample)
					}
				case codexUsageURL:
					if !fact.AllowanceSample.ObservedAt.IsZero() {
						message = codexUsage(fact.AllowanceSample)
					}
				case codexCreditsURL:
					if !fact.ResetCreditsReadAt.IsZero() {
						credits := &t3pb.CreditList{}
						for _, credit := range fact.ResetCredits {
							credits.Credits = append(credits.Credits, &t3pb.Credit{Id: credit.ID, Status: credit.Status, ResetType: credit.ResetType, ExpiresAt: credit.ExpiresAt.UTC().Format(time.RFC3339)})
						}
						message = credits
					}
				}
				if message == nil {
					write(w, &t3pb.ApiResponse{StatusCode: 503, Body: "Account usage is unavailable."})
					return
				}
				body, err := jsonOptions.Marshal(message)
				if err != nil {
					http.Error(w, "cannot encode usage", 500)
					return
				}
				write(w, &t3pb.ApiResponse{StatusCode: 200, Body: string(body)})
				return
			}
			write(w, &t3pb.ApiResponse{StatusCode: 404, Body: "Account unavailable."})
		default:
			http.Error(w, "unsupported management operation", 404)
		}
	})
}
func write(w http.ResponseWriter, message proto.Message) {
	data, err := jsonOptions.Marshal(message)
	if err != nil {
		http.Error(w, "cannot encode response", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
func codexUsage(sample accounts.AllowanceSample) *t3pb.CodexUsage {
	limits := &t3pb.CodexRateLimit{}
	for index, window := range sample.Windows {
		w := &t3pb.CodexWindow{UsedPercent: window.UsedPercent, LimitWindowSeconds: int32(window.DurationSeconds), ResetAt: float64(window.ResetsAt.Unix())}
		if index == 0 {
			limits.PrimaryWindow = w
		} else if index == 1 {
			limits.SecondaryWindow = w
		}
	}
	return &t3pb.CodexUsage{PlanType: sample.Plan, RateLimit: limits}
}
func claudeUsage(sample accounts.AllowanceSample) *t3pb.ClaudeUsage {
	usage := &t3pb.ClaudeUsage{}
	for _, window := range sample.Windows {
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
	return usage
}
