package usage

import "strings"

// nanoUSD stores one billionth of a US dollar. Published token rates are exact
// in this unit.
type nanoUSD int64

// harnessMode is the Codex speed setting a response ran in.
type harnessMode string

const (
	harnessStandard harnessMode = "standard"
	harnessFast     harnessMode = "fast"
)

// API-equivalent cost uses OpenAI's published Codex credit rates, read 11
// October 2026 at https://learn.chatgpt.com/docs/pricing, at 25 credits per US
// dollar, the price of purchased credits. Every rate there is exactly the API's
// Standard price for input, cached input and output
// (https://developers.openai.com/api/docs/pricing), so one rate card serves
// both. Codex credits have no cache-write or long-context charge, and
// purchased Fast usage costs twice the Standard rate.
const fastCostMultiplier = 2

type tokenRates struct {
	input  nanoUSD // per uncached input token
	cached nanoUSD // per cached input token
	output nanoUSD // per output token, reasoning included
}

// creditsPerMillionTokens converts a published rate, in credits per million
// tokens, to nano-US dollars per token: 1 credit per million tokens is 40.
func creditsPerMillionTokens(input, cached, output float64) tokenRates {
	return tokenRates{input: nanoUSD(input * 40), cached: nanoUSD(cached * 40), output: nanoUSD(output * 40)}
}

var apiEquivalentTokenRates = map[string]tokenRates{
	"gpt-6.2-astra": creditsPerMillionTokens(200, 10, 1000),
	"gpt-6.2-sol":   creditsPerMillionTokens(50, 2.5, 250),
	"gpt-6.1-terra": creditsPerMillionTokens(25, 1.25, 125),
	"gpt-6-astra":   creditsPerMillionTokens(250, 25, 1250),
	"gpt-6.1-sol":   creditsPerMillionTokens(50, 2.5, 250),
	"gpt-6-sol":     creditsPerMillionTokens(50, 5, 250),
	"gpt-6-luna":    creditsPerMillionTokens(2.5, 0.25, 12.5),
	"gpt-5.6-sol":   creditsPerMillionTokens(100, 10, 500),
	"gpt-5.6":       creditsPerMillionTokens(100, 10, 500), // alias of gpt-5.6-sol
	"gpt-5.6-terra": creditsPerMillionTokens(50, 5, 300),
	"gpt-5.6-luna":  creditsPerMillionTokens(5, 0.5, 30),
	// Daybreak Blue uses GPT-5.6 Sol credit rates.
	"gpt-daybreak-blue-latest": creditsPerMillionTokens(100, 10, 500),
	"gpt-5.5":                  creditsPerMillionTokens(125, 12.5, 750),
	"gpt-5.5-2026-04-23":       creditsPerMillionTokens(125, 12.5, 750),
	// OpenAI does not publish an auto-review rate. Its daily usage by model
	// charged codex-auto-review exactly at GPT-5.6 Luna rates (1 and 2 October
	// 2026, account 1).
	"codex-auto-review": creditsPerMillionTokens(5, 0.5, 30),
	// Retired from the credit rate card; API Standard prices.
	"gpt-5.4":                 creditsPerMillionTokens(62.5, 6.25, 375),
	"gpt-5.4-2026-03-05":      creditsPerMillionTokens(62.5, 6.25, 375),
	"gpt-5.4-mini":            creditsPerMillionTokens(18.75, 1.875, 112.5),
	"gpt-5.4-mini-2026-03-17": creditsPerMillionTokens(18.75, 1.875, 112.5),
	"gpt-5.3-codex":           creditsPerMillionTokens(43.75, 4.375, 350),
}

// apiEquivalentPrice prices one response. It returns nil only for a model
// missing from the rate card or for inconsistent token counts.
func apiEquivalentPrice(model string, mode harnessMode, usage tokenUsage) *nanoUSD {
	rates, found := apiEquivalentTokenRates[strings.TrimSpace(model)]
	if !found || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return nil
	}
	cost := nanoUSD(usage.InputTokens-usage.CachedInputTokens)*rates.input +
		nanoUSD(usage.CachedInputTokens)*rates.cached +
		nanoUSD(usage.OutputTokens)*rates.output
	if mode == harnessFast {
		cost *= fastCostMultiplier
	}
	return &cost
}
