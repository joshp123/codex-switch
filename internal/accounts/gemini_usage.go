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

// GeminiAccountUsage is a read-only usage snapshot for one Gemini/Antigravity
// account, obtained strictly by running the official `agy -p "/usage"` CLI.
type GeminiAccountUsage struct {
	Key               store.AccountKey
	Email             string
	Plan              string
	State             AccountState
	UnavailableReason string
	AllowanceSample   AllowanceSample
}

// GeminiUsageMonitor periodically reads Gemini allowance from the official
// Antigravity CLI (`agy -p "/usage" --output-format json`). It never reads
// OAuth tokens, replicates login, calls Google APIs directly, or routes
// Gemini traffic.
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

// NewGeminiUsageMonitor constructs a background reader for Antigravity CLI
// usage. Setting CODEX_SWITCH_AGY_BIN=none disables it completely.
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
	sources := discoverGeminiSources(m.homeDir)
	if len(sources) == 0 {
		m.storeAccounts(nil)
		return
	}
	previousByKey := map[store.AccountKey]GeminiAccountUsage{}
	for _, existing := range m.ReadAccounts() {
		previousByKey[existing.Key] = existing
	}

	var next []GeminiAccountUsage
	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		account, present, err := m.readSource(ctx, source, previousByKey)
		if err != nil {
			log.Printf("Gemini usage (%s): %v", source.label(), err)
		}
		if present {
			next = append(next, account)
		}
	}
	m.storeAccounts(next)
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

type geminiAuthSource struct {
	email     string
	tokenPath string // empty for native $HOME/.gemini/antigravity-cli
	fallback  string // distinct key suffix when email is empty
}

func (s geminiAuthSource) label() string {
	if s.email != "" {
		return s.email
	}
	if s.fallback != "" {
		return s.fallback
	}
	return "local"
}

func (s geminiAuthSource) key() store.AccountKey {
	if s.email != "" {
		return AccountKeyFor(ProviderGemini, s.email)
	}
	if s.fallback != "" {
		return store.AccountKey("gemini:" + s.fallback)
	}
	return store.AccountKey("gemini:local")
}

func discoverGeminiSources(homeDir string) []geminiAuthSource {
	if homeDir == "" {
		return nil
	}
	var sources []geminiAuthSource
	seenEmails := map[string]bool{}
	unnamedCount := 0
	add := func(email, tokenPath string) {
		normalized := strings.ToLower(strings.TrimSpace(email))
		fallback := ""
		if normalized != "" {
			if seenEmails[normalized] {
				return
			}
			seenEmails[normalized] = true
		} else {
			unnamedCount++
			if unnamedCount == 1 {
				fallback = "local"
			} else {
				fallback = fmt.Sprintf("local-%d", unnamedCount)
			}
		}
		sources = append(sources, geminiAuthSource{
			email:     strings.TrimSpace(email),
			tokenPath: tokenPath,
			fallback:  fallback,
		})
	}

	// 1. Native agy file-backed login.
	nativeToken := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if info, err := os.Stat(nativeToken); err == nil && !info.IsDir() {
		add(readAntigravityEmailOnly(nativeToken), "")
	}

	// 2. T3 Code Antigravity provider logins.
	pattern := filepath.Join(homeDir, ".t3", "userdata", "providers", "antigravity", "*", "antigravity-acp", "antigravity-oauth-token")
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	for _, match := range matches {
		if info, err := os.Stat(match); err == nil && !info.IsDir() {
			add(readAntigravityEmailOnly(match), match)
		}
	}

	// 3. Native Keychain-backed agy login (no token file on disk, but CLI config dir exists).
	if len(sources) == 0 {
		nativeDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
		if info, err := os.Stat(nativeDir); err == nil && info.IsDir() {
			sources = append(sources, geminiAuthSource{})
		}
	}

	return sources
}

// readAntigravityEmailOnly extracts only the non-secret display email from an
// Antigravity token file; it never decodes or retains any OAuth tokens.
func readAntigravityEmailOnly(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var meta struct {
		IDTokenEmail string `json:"id_token_email"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.IDTokenEmail)
}

func (m *GeminiUsageMonitor) readSource(ctx context.Context, source geminiAuthSource, previousByKey map[store.AccountKey]GeminiAccountUsage) (GeminiAccountUsage, bool, error) {
	output, err := runAgyUsageCLI(ctx, m.agyExecutable, source.tokenPath)
	key := source.key()
	if errors.Is(err, errGeminiAuthRequired) {
		if source.email == "" && source.tokenPath == "" {
			// Native directory existed without an active login; omit silently.
			return GeminiAccountUsage{}, false, nil
		}
		account := GeminiAccountUsage{
			Key:               key,
			Email:             source.email,
			Plan:              "Antigravity",
			State:             AccountSignInRequired,
			UnavailableReason: "sign in to Antigravity again",
		}
		return account, true, nil
	}
	if err != nil {
		if prev, ok := previousByKey[key]; ok {
			prev.State = AccountUsageCheckFailed
			prev.UnavailableReason = "Antigravity usage check failed"
			return prev, true, err
		}
		if source.email == "" && source.tokenPath == "" {
			return GeminiAccountUsage{}, false, err
		}
		return GeminiAccountUsage{
			Key:               key,
			Email:             source.email,
			Plan:              "Antigravity",
			State:             AccountUsageCheckFailed,
			UnavailableReason: "Antigravity usage check failed",
		}, true, err
	}

	sample, err := parseAgyUsageJSON(output, m.now().UTC())
	if err != nil {
		if prev, ok := previousByKey[key]; ok {
			prev.State = AccountUsageCheckFailed
			prev.UnavailableReason = "Antigravity usage check failed"
			return prev, true, err
		}
		return GeminiAccountUsage{}, false, err
	}

	state := AccountReady
	if sample.LimitReached {
		state = AccountLimited
	}
	return GeminiAccountUsage{
		Key:               key,
		Email:             source.email,
		Plan:              "Antigravity",
		State:             state,
		UnavailableReason: "usage only",
		AllowanceSample:   sample,
	}, true, nil
}

func runAgyUsageCLI(parent context.Context, agyBin, tokenPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, geminiCLIReadTimeout)
	defer cancel()

	env := os.Environ()
	if tokenPath != "" {
		tempHome, err := os.MkdirTemp("", "codex-switch-agy-*")
		if err != nil {
			return nil, fmt.Errorf("create temp home: %w", err)
		}
		defer os.RemoveAll(tempHome)
		cliDir := filepath.Join(tempHome, ".gemini", "antigravity-cli")
		if err := os.MkdirAll(cliDir, 0o700); err != nil {
			return nil, fmt.Errorf("create temp agy dir: %w", err)
		}
		if err := os.Symlink(tokenPath, filepath.Join(cliDir, "antigravity-oauth-token")); err != nil {
			return nil, fmt.Errorf("link agy token file: %w", err)
		}
		env = withEnvOverrides(env, map[string]string{
			"HOME":           tempHome,
			"SSH_CONNECTION": "1",
		})
	}

	cmd := exec.CommandContext(ctx, agyBin, "-p", "/usage", "--output-format", "json")
	cmd.Env = env
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

func (b *agyOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

func withEnvOverrides(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}
