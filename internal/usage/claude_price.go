package usage

import (
	"slices"
	"strings"
	"time"
)

// Anthropic's Standard rates, checked 9 November 2026:
// https://platform.claude.com/docs/en/about-claude/pricing.
// One dollar per million tokens is 1,000 nano-US dollars per token.
var claudeTokenRates = map[string]tokenRates{
	"claude-fable-5-2":  {8000, 200, 40000},
	"claude-mythos-5-2": {8000, 200, 40000},
	"claude-fable-5-1":  {10000, 250, 50000},
	"claude-mythos-5-1": {10000, 250, 50000},
	"claude-opus-5-5":   {4000, 200, 20000},
	"claude-sonnet-5-5": {2000, 100, 10000},
	"claude-haiku-5-5":  {100, 10, 500},
	"claude-fable-5":    {10000, 1000, 50000},
	"claude-mythos-5":   {10000, 1000, 50000},
	"claude-opus-5":     {5000, 500, 25000},
	"claude-opus-4-8":   {5000, 500, 25000},
	"claude-opus-4-7":   {5000, 500, 25000},
	"claude-opus-4-6":   {5000, 500, 25000},
	"claude-opus-4-5":   {5000, 500, 25000},
	"claude-opus-4-1":   {15000, 1500, 75000},
	"claude-opus-4":     {15000, 1500, 75000},
	"claude-sonnet-5":   {2000, 200, 10000},
	"claude-sonnet-4-6": {3000, 300, 15000},
	"claude-sonnet-4-5": {3000, 300, 15000},
	"claude-sonnet-4":   {3000, 300, 15000},
	"claude-haiku-4-5":  {1000, 100, 5000},
	"claude-haiku-3-5":  {800, 80, 4000},
}

// claudeFastModeModels are the models Anthropic serves in fast mode, at twice
// their Standard rates.
var claudeFastModeModels = []string{"claude-fable-5-2", "claude-mythos-5-2", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8"}

func claudeMessagePrice(model, speed, inferenceGeo string, usage claudeMessageUsage) *int64 {
	model = strings.TrimSpace(model)
	if len(model) > 9 && model[len(model)-9] == '-' {
		if _, err := time.Parse("20060102", model[len(model)-8:]); err == nil {
			model = model[:len(model)-9]
		}
	}
	if model == "claude-3-5-haiku" {
		model = "claude-haiku-3-5"
	}
	rates, found := claudeTokenRates[model]
	if !found || usage.Input == nil || usage.Output == nil {
		return nil
	}
	input, output := *usage.Input, *usage.Output
	cacheRead, cacheWrite := claudeTokenCount(usage.CacheRead), claudeTokenCount(usage.CacheWrite)
	if input < 0 || output < 0 || cacheRead < 0 || cacheWrite < 0 {
		return nil
	}
	if model == "claude-haiku-5-5" && input+cacheRead+cacheWrite > 100000 {
		rates = tokenRates{500, 50, 2500}
	}
	writes5m, writes1h := cacheWrite, int64(0)
	if usage.CacheCreation != nil {
		writes5m, writes1h = usage.CacheCreation.FiveMinutes, usage.CacheCreation.OneHour
		if writes5m < 0 || writes1h < 0 || writes5m+writes1h != cacheWrite {
			return nil
		}
	}
	if speed == "fast" && slices.Contains(claudeFastModeModels, model) {
		rates.input *= 2
		rates.cached *= 2
		rates.output *= 2
	}
	cost := nanoUSD(input)*rates.input + nanoUSD(cacheRead)*rates.cached + nanoUSD(output)*rates.output +
		nanoUSD(writes5m)*rates.input*5/4 + nanoUSD(writes1h)*rates.input*2
	if inferenceGeo == "us" {
		cost = cost * 11 / 10
	}
	if usage.ServerToolUse != nil {
		cost += nanoUSD(usage.ServerToolUse.WebSearchRequests) * 10000000
	}
	result := int64(cost)
	return &result
}
