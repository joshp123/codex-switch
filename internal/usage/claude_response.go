package usage

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

type claudeCacheCreation struct {
	FiveMinutes int64 `json:"ephemeral_5m_input_tokens"`
	OneHour     int64 `json:"ephemeral_1h_input_tokens"`
}

type claudeMessageUsage struct {
	Input         *int64               `json:"input_tokens"`
	Output        *int64               `json:"output_tokens"`
	CacheRead     *int64               `json:"cache_read_input_tokens"`
	CacheWrite    *int64               `json:"cache_creation_input_tokens"`
	CacheCreation *claudeCacheCreation `json:"cache_creation"`
	Speed         string               `json:"speed"`
	InferenceGeo  string               `json:"inference_geo"`
	ServerToolUse *struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// ClaudeResponseUsage reads accounting fields from native Messages API payloads.
// Content and tool arguments never enter the ledger.
type ClaudeResponseUsage struct {
	model, speed, inferenceGeo, responseID string
	usage                                  claudeMessageUsage
	complete                               bool
}

func NewClaudeResponseUsage(model, speed, inferenceGeo string) *ClaudeResponseUsage {
	return &ClaudeResponseUsage{model: model, speed: speed, inferenceGeo: inferenceGeo}
}

// Observe accepts a JSON response or one SSE data payload. Delta counts are
// cumulative; omitted fields retain the value from message_start.
func (observation *ClaudeResponseUsage) Observe(payload []byte) (bool, error) {
	var event struct {
		Type    string             `json:"type"`
		ID      string             `json:"id"`
		Model   string             `json:"model"`
		Usage   claudeMessageUsage `json:"usage"`
		Message struct {
			ID    string             `json:"id"`
			Model string             `json:"model"`
			Usage claudeMessageUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return false, err
	}
	if observation.complete {
		return false, nil
	}
	switch event.Type {
	case "message_start":
		observation.responseID, observation.model, observation.usage = event.Message.ID, event.Message.Model, event.Message.Usage
	case "message_delta":
		mergeClaudeUsage(&observation.usage, event.Usage)
	case "message":
		observation.responseID, observation.model, observation.usage = event.ID, event.Model, event.Usage
		observation.complete = true
	case "message_stop":
		observation.complete = true
	}
	return observation.complete, nil
}

func mergeClaudeUsage(usage *claudeMessageUsage, delta claudeMessageUsage) {
	if delta.Input != nil {
		usage.Input = delta.Input
	}
	if delta.Output != nil {
		usage.Output = delta.Output
	}
	if delta.CacheRead != nil {
		usage.CacheRead = delta.CacheRead
	}
	if delta.CacheWrite != nil {
		usage.CacheWrite = delta.CacheWrite
	}
	if delta.CacheCreation != nil {
		usage.CacheCreation = delta.CacheCreation
	}
	if delta.Speed != "" {
		usage.Speed = delta.Speed
	}
	if delta.InferenceGeo != "" {
		usage.InferenceGeo = delta.InferenceGeo
	}
	if delta.ServerToolUse != nil {
		usage.ServerToolUse = delta.ServerToolUse
	}
}

// Record normalises Anthropic's disjoint input categories into the shared
// ledger's inclusive input count, preserving the cache subsets.
func (observation *ClaudeResponseUsage) Record(taskID store.TaskID, account store.AccountKey, completedAt time.Time) (store.CompletedResponseRecord, error) {
	if !observation.complete || observation.responseID == "" || observation.usage.Input == nil || observation.usage.Output == nil {
		return store.CompletedResponseRecord{}, errors.New("Claude response is missing completed usage")
	}
	usage := observation.usage
	if usage.Speed != "" {
		observation.speed = usage.Speed
	}
	if usage.InferenceGeo != "" {
		observation.inferenceGeo = usage.InferenceGeo
	}
	cacheWrite := claudeTokenCount(usage.CacheWrite)
	record := store.CompletedResponseRecord{
		CompletedResponseID: store.CompletedResponseID("claude:" + observation.responseID),
		TaskID:              taskID, AccountKey: account, ModelSlug: observation.model, HarnessMode: observation.speed,
		InputTokens:       *usage.Input + claudeTokenCount(usage.CacheRead) + cacheWrite,
		CachedInputTokens: claudeTokenCount(usage.CacheRead), CacheWriteInputTokens: &cacheWrite,
		OutputTokens: *usage.Output, CompletedAt: completedAt,
		CostNanoUSD: claudeMessagePrice(observation.model, observation.speed, observation.inferenceGeo, usage),
	}
	return record, nil
}

func claudeTokenCount(count *int64) int64 {
	if count == nil {
		return 0
	}
	return *count
}
