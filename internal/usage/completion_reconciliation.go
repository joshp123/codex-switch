package usage

import (
	"fmt"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// reconciledCompletion combines one native usage event with its serving response when available.
type reconciledCompletion struct {
	TaskID            string
	At                time.Time
	LastCompletedAt   time.Time
	AccountKey        store.AccountKey
	ProviderAccountID store.ProviderAccountID
	Model             string
	Tokens            int64
	Cost              *nanoUSD
	Fast              bool
	NativeSource      *sessionUsageObservation
	ResponseID        store.CompletedResponseID
}

func nativeCompletion(observation *sessionUsageObservation, proxy *store.CompletedResponseRecord) (reconciledCompletion, error) {
	usage := *observation.Usage
	completion := reconciledCompletion{TaskID: observation.TaskID, At: observation.At, LastCompletedAt: observation.At, Model: observation.Model, Tokens: usage.InputTokens + usage.OutputTokens, NativeSource: observation, ResponseID: store.CompletedResponseID(observation.ResponseID)}
	mode := harnessStandard
	if observation.ServiceTier != nil && (*observation.ServiceTier == "priority" || *observation.ServiceTier == "fast") {
		mode = harnessFast
	}
	if proxy != nil {
		if usage.InputTokens != proxy.InputTokens || usage.CachedInputTokens != proxy.CachedInputTokens || usage.OutputTokens != proxy.OutputTokens || usage.ReasoningOutputTokens != proxy.ReasoningTokens {
			return reconciledCompletion{}, fmt.Errorf("token counts disagree for response %s", completion.ResponseID)
		}
		completion.AccountKey = proxy.AccountKey
		completion.Model = proxy.ModelSlug
		// A task's latest completion uses the latest proxy or native response
		// timestamp. Its usage date still comes from the native timestamp.
		if proxy.CompletedAt.After(completion.LastCompletedAt) {
			completion.LastCompletedAt = proxy.CompletedAt
		}
		// Either record saying Codex requested Fast means Fast. Proxies
		// before October 2026 took the speed from OpenAI's response,
		// which reports "default" for Fast work OpenAI bills as Fast.
		if proxy.HarnessMode == string(harnessFast) {
			mode = harnessFast
		}
	}
	completion.Fast = mode == harnessFast
	completion.Cost = apiEquivalentPrice(completion.Model, mode, usage)
	if proxy != nil && proxy.CostNanoUSD != nil {
		cost := nanoUSD(*proxy.CostNanoUSD)
		completion.Cost = &cost
	}
	return completion, nil
}

func proxyCompletion(response store.CompletedResponseRecord) reconciledCompletion {
	usage := tokenUsage{InputTokens: response.InputTokens, CachedInputTokens: response.CachedInputTokens, CacheWriteInputTokens: response.CacheWriteInputTokens, OutputTokens: response.OutputTokens, ReasoningOutputTokens: response.ReasoningTokens}
	mode := harnessMode(response.HarnessMode)
	cost := apiEquivalentPrice(response.ModelSlug, mode, usage)
	if response.CostNanoUSD != nil {
		price := nanoUSD(*response.CostNanoUSD)
		cost = &price
	}
	return reconciledCompletion{TaskID: string(response.TaskID), At: response.CompletedAt, LastCompletedAt: response.CompletedAt, AccountKey: response.AccountKey, Model: response.ModelSlug, Tokens: response.InputTokens + response.OutputTokens, Cost: cost, Fast: mode == harnessFast, ResponseID: response.CompletedResponseID}
}
