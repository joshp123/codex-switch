package accounts

import (
	"strconv"
	"strings"
)

// builtInClaudeCodeVersion is the Claude Code release the proxy reads
// Anthropic's usage as until a newer Claude Code sends a request. Anthropic
// lists an account's stored resets only to the Claude Code command line, and
// only to a release new enough: to any other client it answers that the
// account holds none, with the reason surface or cli_version.
const builtInClaudeCodeVersion = "2.1.321"

// ObserveClaudeClient notes the client that sent a Claude request. The proxy
// reads usage as the newest Claude Code it has served, so a Claude Code
// upgrade keeps the reads current without a Codex Switch release.
func (monitor *Monitor) ObserveClaudeClient(userAgent string) {
	product, _, _ := strings.Cut(strings.TrimSpace(userAgent), " ")
	version, isClaudeCode := strings.CutPrefix(product, "claude-cli/")
	if !isClaudeCode || len(releaseNumbers(version)) == 0 {
		return
	}
	for {
		observed := monitor.observedClaudeCodeVersion.Load()
		newest := builtInClaudeCodeVersion
		if observed != nil {
			newest = *observed
		}
		if !newerRelease(version, newest) || monitor.observedClaudeCodeVersion.CompareAndSwap(observed, &version) {
			return
		}
	}
}

// claudeCodeVersion is the newest Claude Code release the proxy knows.
func (monitor *Monitor) claudeCodeVersion() string {
	if observed := monitor.observedClaudeCodeVersion.Load(); observed != nil {
		return *observed
	}
	return builtInClaudeCodeVersion
}

// claudeCodeUserAgent is the User-Agent of the Claude Code command line at
// that release.
func (monitor *Monitor) claudeCodeUserAgent() string {
	return "claude-cli/" + monitor.claudeCodeVersion() + " (external, cli)"
}

// releaseNumbers reads a dotted release such as 2.1.321. It is empty for
// anything else.
func releaseNumbers(version string) []int {
	var numbers []int
	for part := range strings.SplitSeq(version, ".") {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return nil
		}
		numbers = append(numbers, number)
	}
	return numbers
}

func newerRelease(version, than string) bool {
	left, right := releaseNumbers(version), releaseNumbers(than)
	for index := range min(len(left), len(right)) {
		if left[index] != right[index] {
			return left[index] > right[index]
		}
	}
	return len(left) > len(right)
}
