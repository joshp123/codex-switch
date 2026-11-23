package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

const (
	defaultGeminiUsageInterval = 5 * time.Minute
	geminiCLIReadTimeout       = 15 * time.Second
	geminiCLIOutputLimit       = 1 << 20
)

var errGeminiAuthRequired = errors.New("Antigravity CLI requires sign-in")

// GeminiAccountUsage is a read-only usage snapshot for the native Antigravity
// CLI login, obtained strictly by running `agy -p "/usage" --output-format json`.
type GeminiAccountUsage struct {
	Key               store.AccountKey
	Email             string
	Plan              string
	State             AccountState
	UnavailableReason string
	AllowanceSample   AllowanceSample
}

// GeminiUsageMonitor periodically reads Gemini allowance from the official
// Antigravity CLI (`agy -p "/usage" --output-format json`) using the user's
// native CLI session. It never reads OAuth token files, replicates login,
// calls Google APIs directly, or routes Gemini traffic.
type GeminiUsageMonitor struct {
	homeDir       string
	agyExecutable string
	interval      time.Duration
	now           func() time.Time
	onChange      func()

	mu            sync.RWMutex
	accounts      []GeminiAccountUsage
	lastSignature string
}

// NewGeminiUsageMonitor constructs a background reader for native Antigravity
// CLI usage. Setting CODEX_SWITCH_AGY_BIN=none disables it completely.
func NewGeminiUsageMonitor(onChange func()) *GeminiUsageMonitor {
	home, _ := os.UserHomeDir()
	return &GeminiUsageMonitor{
		homeDir:       home,
		agyExecutable: findAgyExecutable(home),
		interval:      defaultGeminiUsageInterval,
		now:           time.Now,
		onChange:      onChange,
	}
}

func (m *GeminiUsageMonitor) SetInterval(interval time.Duration) {
	if interval > 0 {
		m.interval = interval
	}
}

// ReadAccounts returns the latest cached Gemini usage snapshots without
// blocking on the CLI.
func (m *GeminiUsageMonitor) ReadAccounts() []GeminiAccountUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.accounts) == 0 {
		return nil
	}
	copied := make([]GeminiAccountUsage, len(m.accounts))
	for i, account := range m.accounts {
		account.AllowanceSample.Windows = slices.Clone(account.AllowanceSample.Windows)
		copied[i] = account
	}
	return copied
}

// Run refreshes Gemini usage immediately in the background and then on each
// interval until ctx is cancelled.
func (m *GeminiUsageMonitor) Run(ctx context.Context) {
	if m.agyExecutable == "" {
		return
	}
	for {
		m.refreshOnce(ctx)
		timer := time.NewTimer(m.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *GeminiUsageMonitor) refreshOnce(ctx context.Context) {
	if m.agyExecutable == "" || ctx.Err() != nil {
		return
	}
	previous := m.ReadAccounts()
	var prevAccount *GeminiAccountUsage
	if len(previous) > 0 {
		prevAccount = &previous[0]
	}

	account, present, err := m.readNativeCLI(ctx, prevAccount)
	if err != nil {
		log.Printf("Gemini usage: %v", err)
	}
	if !present {
		m.storeAccounts(nil)
		return
	}
	m.storeAccounts([]GeminiAccountUsage{account})
}

func (m *GeminiUsageMonitor) storeAccounts(accounts []GeminiAccountUsage) {
	sigBytes, _ := json.Marshal(accounts)
	signature := string(sigBytes)
	m.mu.Lock()
	changed := signature != m.lastSignature
	m.accounts = accounts
	m.lastSignature = signature
	onChange := m.onChange
	m.mu.Unlock()
	if changed && onChange != nil {
		onChange()
	}
}

func (m *GeminiUsageMonitor) readNativeCLI(ctx context.Context, prev *GeminiAccountUsage) (GeminiAccountUsage, bool, error) {
	output, err := runAgyUsageCLI(ctx, m.agyExecutable)
	const key = store.AccountKey("gemini:local")
	if errors.Is(err, errGeminiAuthRequired) {
		if prev != nil {
			return GeminiAccountUsage{
				Key:               key,
				Plan:              "Antigravity",
				State:             AccountSignInRequired,
				UnavailableReason: "sign in to Antigravity again",
			}, true, nil
		}
		return GeminiAccountUsage{}, false, nil
	}
	if err != nil {
		if prev != nil {
			stale := *prev
			stale.State = AccountUsageCheckFailed
			stale.UnavailableReason = "Antigravity usage check failed"
			return stale, true, err
		}
		return GeminiAccountUsage{}, false, err
	}

	sample, err := parseAgyUsageJSON(output, m.now().UTC())
	if err != nil {
		if prev != nil {
			stale := *prev
			stale.State = AccountUsageCheckFailed
			stale.UnavailableReason = "Antigravity usage check failed"
			return stale, true, err
		}
		return GeminiAccountUsage{}, false, err
	}

	state := AccountReady
	if sample.LimitReached {
		state = AccountLimited
	}
	return GeminiAccountUsage{
		Key:               key,
		Plan:              "Antigravity",
		State:             state,
		UnavailableReason: "usage only",
		AllowanceSample:   sample,
	}, true, nil
}

func runAgyUsageCLI(parent context.Context, agyBin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, geminiCLIReadTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agyBin, "-p", "/usage", "--output-format", "json")
	cmd.Env = cleanAgyEnv(os.Environ())
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 200 * time.Millisecond
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var authRequired atomic.Bool
	stdoutBuf := &agyOutputBuffer{limit: geminiCLIOutputLimit, authRequired: &authRequired, cancel: cancel}
	stderrBuf := &agyOutputBuffer{limit: geminiCLIOutputLimit, authRequired: &authRequired, cancel: cancel}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agy: %w", err)
	}
	waitErr := cmd.Wait()
	if cmd.Process != nil {
		// Unconditionally reap any child still alive in the process group after Wait returns.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if authRequired.Load() {
		return nil, errGeminiAuthRequired
	}
	if waitErr != nil {
		return nil, fmt.Errorf("agy /usage failed: %w", waitErr)
	}
	return stdoutBuf.Bytes(), nil
}

type agyOutputBuffer struct {
	mu           sync.Mutex
	buf          bytes.Buffer
	limit        int
	authRequired *atomic.Bool
	cancel       context.CancelFunc
}

func (b *agyOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.buf.Len() < b.limit {
		chunk := p
		if remaining := b.limit - b.buf.Len(); len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		_, _ = b.buf.Write(chunk)
	}
	needsAuth := bytes.Contains(b.buf.Bytes(), []byte("Authentication required"))
	b.mu.Unlock()
	if needsAuth {
		b.authRequired.Store(true)
		b.cancel()
	}
	return len(p), nil
}

func (b *agyOutputBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

type agyUsageEnvelope struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Groups []agyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

type agyUsageGroup struct {
	Name    string           `json:"name"`
	Buckets []agyUsageBucket `json:"buckets"`
}

type agyUsageBucket struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Window            string   `json:"window"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time"`
}

func parseAgyUsageJSON(raw []byte, observedAt time.Time) (AllowanceSample, error) {
	trimmed := bytes.TrimSpace(raw)
	if idx := bytes.IndexByte(trimmed, '{'); idx > 0 {
		trimmed = trimmed[idx:]
	}
	var envelope agyUsageEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return AllowanceSample{}, fmt.Errorf("decode agy /usage JSON: %w", err)
	}
	if envelope.Status != "SUCCESS" || envelope.Command.Name != "usage" {
		return AllowanceSample{}, fmt.Errorf("unexpected agy /usage response: status=%q command=%q", envelope.Status, envelope.Command.Name)
	}

	var windows []ReportedWindow
	primaryLimitReached := false
	for _, group := range envelope.Command.Data.Groups {
		modelID, modelName := geminiGroupIdentity(group.Name)
		for _, bucket := range group.Buckets {
			period := geminiWindowDuration(bucket.Window)
			if period <= 0 || bucket.RemainingFraction == nil || math.IsNaN(*bucket.RemainingFraction) || math.IsInf(*bucket.RemainingFraction, 0) {
				continue
			}
			resetAt, err := parseGeminiResetTime(bucket.ResetTime)
			if err != nil || resetAt.IsZero() {
				continue
			}
			usedPercent := math.Min(100, math.Max(0, (1-*bucket.RemainingFraction)*100))
			bModelID, bModelName := modelID, modelName
			if strings.HasPrefix(bucket.ID, "gemini-") {
				bModelID, bModelName = "", ""
			} else if strings.HasPrefix(bucket.ID, "3p-") {
				bModelID, bModelName = "3p", "Claude & GPT"
			}
			if bModelID == "" && usedPercent >= 100 {
				primaryLimitReached = true
			}
			windows = append(windows, ReportedWindow{
				DurationSeconds: period,
				UsedPercent:     usedPercent,
				ResetsAt:        resetAt.UTC(),
				ModelID:         bModelID,
				ModelName:       bModelName,
			})
		}
	}
	if len(windows) == 0 {
		return AllowanceSample{}, errors.New("agy /usage returned no allowance windows")
	}
	sort.SliceStable(windows, func(i, j int) bool {
		if (windows[i].ModelID != "") != (windows[j].ModelID != "") {
			return windows[i].ModelID == ""
		}
		if windows[i].ModelID != windows[j].ModelID {
			return windows[i].ModelID < windows[j].ModelID
		}
		return windows[i].DurationSeconds < windows[j].DurationSeconds
	})
	return AllowanceSample{
		ObservedAt:   observedAt,
		Plan:         "Antigravity",
		Allowed:      !primaryLimitReached,
		LimitReached: primaryLimitReached,
		Windows:      windows,
	}, nil
}

func geminiGroupIdentity(groupName string) (modelID, modelName string) {
	lower := strings.ToLower(strings.TrimSpace(groupName))
	switch {
	case strings.Contains(lower, "gemini"):
		return "", ""
	case strings.Contains(lower, "claude") || strings.Contains(lower, "gpt"):
		return "3p", "Claude & GPT"
	default:
		if lower == "" {
			return "", ""
		}
		return lower, strings.TrimSpace(groupName)
	}
}

func geminiWindowDuration(window string) int64 {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h":
		return 5 * 3600
	case "weekly", "7d":
		return 7 * 86400
	case "daily", "24h", "1d":
		return 86400
	default:
		return 0
	}
}

func parseGeminiResetTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty reset_time")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, value)
}

func findAgyExecutable(homeDir string) string {
	if configured, set := os.LookupEnv("CODEX_SWITCH_AGY_BIN"); set {
		configured = strings.TrimSpace(configured)
		if configured == "" || strings.EqualFold(configured, "none") || strings.EqualFold(configured, "off") {
			return ""
		}
		return configured
	}
	if found, err := exec.LookPath("agy"); err == nil {
		return found
	}
	if homeDir != "" {
		candidate := filepath.Join(homeDir, ".local", "bin", "agy")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// cleanAgyEnv strips inherited ANTIGRAVITY_* harness variables and SSH session
// markers so `agy` always runs against this Mac's native CLI Keychain login.
func cleanAgyEnv(base []string) []string {
	cleaned := make([]string, 0, len(base))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ANTIGRAVITY_") || strings.HasPrefix(name, "SSH_") {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}
