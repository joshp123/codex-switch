package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/joshp123/codex-switch/internal/taskidentity"
)

var errNotPiSession = errors.New("not a Pi session")

var standardPiSessionFilename = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{3}Z_([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)\.jsonl$`)

type piSessionHeader struct {
	Type          string    `json:"type"`
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	ParentSession string    `json:"parentSession"`
}

func readPiSessionHeader(path string) (piSessionHeader, error) {
	var header piSessionHeader
	file, err := os.Open(path)
	if err != nil {
		return header, err
	}
	defer file.Close()
	line, err := bufio.NewReader(file).ReadBytes('\n')
	if err != nil {
		return header, err
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err = json.Unmarshal(line, &kind); err != nil {
		return header, fmt.Errorf("parse Pi session metadata %s: %w", path, err)
	}
	if kind.Type != "session" {
		return header, errNotPiSession
	}
	if err = json.Unmarshal(line, &header); err != nil {
		return header, fmt.Errorf("parse Pi session metadata %s: %w", path, err)
	}
	if header.Type != "session" || header.ID == "" {
		return header, fmt.Errorf("missing Pi session metadata: %s", path)
	}
	return header, nil
}

func (ledger *UsageLedger) discoverPiSessions(sessionDirectory string, since time.Time) ([]sessionFile, error) {
	var sources []sessionFile
	err := filepath.WalkDir(sessionDirectory, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.ModTime().Before(since) {
			return nil
		}
		header, err := ledger.cachedPiSessionHeader(path)
		if err == io.EOF || errors.Is(err, errNotPiSession) || os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		identity := fmt.Sprintf("%d", info.ModTime().UnixNano())
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
		}
		sources = append(sources, sessionFile{Path: path, TaskID: taskidentity.PiSession(header.ID), Size: info.Size(), ModifiedAt: info.ModTime(), Identity: identity})
		return nil
	})
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, err
}

type piSessionLine struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	CustomType string    `json:"customType"`
	Data       struct {
		SessionFile string `json:"session_file"`
	} `json:"data"`
	Message struct {
		Role       string `json:"role"`
		Provider   string `json:"provider"`
		ResponseID string `json:"responseId"`
		Model      string `json:"model"`
		Usage      *struct {
			Input      int64 `json:"input"`
			Cached     int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Output     int64 `json:"output"`
			Reasoning  int64 `json:"reasoning"`
			Total      int64 `json:"totalTokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (ledger *UsageLedger) readPiSession(ctx context.Context, source sessionFile, cursor sessionReadCursor) (sessionReadResult, error) {
	result := sessionReadResult{Cursor: cursor}
	header, err := ledger.cachedPiSessionHeader(source.Path)
	if err != nil {
		return result, err
	}
	if taskidentity.PiSession(header.ID) != source.TaskID {
		return result, fmt.Errorf("Pi session identity changed: %s", source.Path)
	}
	result.Metadata = taskMetadata{TaskID: source.TaskID, CreatedAt: header.Timestamp, SourceKind: piSessionSourceKind}
	if header.ParentSession != "" {
		parent, err := ledger.cachedPiSessionHeader(header.ParentSession)
		if os.IsNotExist(err) && cursor.KnownParentTaskID != "" {
			result.Metadata.ParentTaskID = cursor.KnownParentTaskID
		} else if filename := standardPiSessionFilename.FindStringSubmatch(filepath.Base(header.ParentSession)); os.IsNotExist(err) && filename != nil {
			result.Metadata.ParentTaskID = taskidentity.PiSession(filename[1])
		} else if err != nil {
			return result, fmt.Errorf("read Pi parent session: %w", err)
		} else {
			result.Metadata.ParentTaskID = taskidentity.PiSession(parent.ID)
		}
		result.Cursor.KnownParentTaskID = result.Metadata.ParentTaskID
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if _, err = file.Seek(cursor.Offset, io.SeekStart); err != nil {
		return result, err
	}
	reader := bufio.NewReader(io.LimitReader(file, source.Size-cursor.Offset))
	for {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		offset := result.Cursor.Offset
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, err
		}
		result.Cursor.Offset += int64(len(line))
		if !bytes.Contains(line, []byte(`"usage"`)) && !bytes.Contains(line, []byte(`subagent-record`)) {
			continue
		}
		var record piSessionLine
		if err = json.Unmarshal(line, &record); err != nil {
			return result, fmt.Errorf("parse Pi usage %s at %d: %w", source.Path, offset, err)
		}
		if header.ParentSession != "" && record.Timestamp.Before(header.Timestamp) {
			continue
		}
		if record.Type == "custom" && (record.CustomType == "subagent-record" || record.CustomType == "luna:subagent-record") && record.Data.SessionFile != "" {
			child, err := ledger.cachedPiSessionHeader(record.Data.SessionFile)
			if filename := standardPiSessionFilename.FindStringSubmatch(filepath.Base(record.Data.SessionFile)); os.IsNotExist(err) && filename != nil {
				child.ID = filename[1]
			} else if err != nil {
				return result, fmt.Errorf("read Pi child session: %w", err)
			}
			result.ChildTasks = append(result.ChildTasks, childTaskRecord{TaskID: taskidentity.PiSession(child.ID), SourceOffset: offset})
			continue
		}
		message := record.Message
		if record.Type != "message" || message.Role != "assistant" || message.Provider != "openai-codex" || message.Usage == nil {
			continue
		}
		usage := message.Usage
		input := usage.Input + usage.Cached + usage.CacheWrite
		if input == 0 && usage.Output == 0 {
			continue
		}
		if message.ResponseID == "" {
			return result, fmt.Errorf("Pi usage has no response identity: %s at %d", source.Path, offset)
		}
		if usage.Input < 0 || usage.Cached < 0 || usage.CacheWrite < 0 || usage.Output < 0 || usage.Reasoning < 0 || usage.Reasoning > usage.Output || input+usage.Output != usage.Total {
			return result, fmt.Errorf("invalid Pi token usage: %s at %d", source.Path, offset)
		}
		tokens := &tokenUsage{InputTokens: input, CachedInputTokens: usage.Cached, OutputTokens: usage.Output, ReasoningOutputTokens: usage.Reasoning, TotalTokens: usage.Total}
		// Pi converts an absent provider cache-write count to zero and omits service tier.
		if usage.CacheWrite > 0 {
			tokens.CacheWriteInputTokens = &usage.CacheWrite
		}
		result.Observations = append(result.Observations, sessionUsageObservation{SourceKind: piSessionSourceKind, ResponseID: message.ResponseID, TaskID: source.TaskID, SourcePath: source.Path, SourceOffset: offset, At: record.Timestamp, Model: message.Model, Usage: tokens})
	}
	return result, nil
}
