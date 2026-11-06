package store

import "time"

// AccountKey identifies a subscription account by its provider and email,
// such as "chatgpt:you@example.com", so every Mac records an account under the
// same key.
type AccountKey string

// TaskID makes the affinity key impossible to swap with another string identifier.
type TaskID string

// WorkKind prevents metadata requests from creating or moving model-turn pins.
type WorkKind string

const (
	ModelTurnWorkKind    WorkKind = "model_turn"
	TaskMetadataWorkKind WorkKind = "task_metadata"
)

// TaskAccountPin is durable so draining proxy generations agree on the next account.
type TaskAccountPin struct {
	TaskID     TaskID
	AccountKey AccountKey
	PinnedAt   time.Time
}

// CompletedResponseID is the provider identity used to record one completion exactly once.
type CompletedResponseID string

// CompletedResponseRecord excludes response content because accounting needs token facts only.
type CompletedResponseRecord struct {
	CompletedResponseID   CompletedResponseID
	TaskID                TaskID
	AccountKey            AccountKey
	ModelSlug             string
	HarnessMode           string
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens *int64
	CostNanoUSD           *int64
	OutputTokens          int64
	ReasoningTokens       int64
	CompletedAt           time.Time
}
