package usage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
)

// Claude names come from its running-session registry and session files.
// Usage comes from served responses.
func (ledger *UsageLedger) visitClaudeTaskMetadata(taskIDs iter.Seq[string], visit func(taskMetadata)) error {
	var ids []string
	seen := make(map[string]bool)
	sessions := map[string]map[string]bool{"code": {}, "pi": {}}
	for id := range taskIDs {
		if seen[id] || !strings.HasPrefix(id, "claude:") {
			continue
		}
		seen[id] = true
		client, session, found := strings.Cut(strings.TrimPrefix(id, "claude:"), ":")
		if !found || session == "" || filepath.Base(session) != session {
			continue
		}
		ids = append(ids, id)
		if sessions[client] != nil {
			sessions[client][session] = true
		}
	}
	titlesBySession := make(map[string]string)
	pathsByClient := make(map[string]map[string][]string)
	for _, client := range []string{"code", "pi"} {
		if len(sessions[client]) == 0 {
			continue
		}
		var directory string
		var err error
		if client == "code" {
			titlesBySession, err = ledger.claudeCodeTaskTitles(sessions[client])
			if err != nil {
				return err
			}
			continue
		}
		directory, err = piSessionDirectory()
		if err != nil {
			return err
		}
		if ledger.piDiscovery.Started && ledger.piDiscovery.Directory == directory {
			pathsByClient[client], err = ledger.discoveredPiTitlePaths(sessions[client])
		} else {
			pathsByClient[client], err = ledger.cachedTitlePaths(directory, true, sessions[client])
		}
		if err != nil {
			return err
		}
	}
	for _, id := range ids {
		client, session, _ := strings.Cut(strings.TrimPrefix(id, "claude:"), ":")
		item := taskMetadata{TaskID: id, SourceKind: "claude:" + client}
		if client == "code" {
			item.Title = titlesBySession[session]
		}
		for _, path := range pathsByClient[client][session] {
			var err error
			item.Title, err = ledger.piSessionTitle(path)
			if err != nil {
				return err
			}
		}
		visit(item)
	}
	return nil
}

type claudeRegistrySession struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
}

type cachedClaudeRegistryEntry struct {
	Fingerprint sessionFileFingerprint
	Session     claudeRegistrySession
}

type claudeTitleSource struct {
	Path        string
	Fingerprint sessionFileFingerprint
}

type cachedClaudeTaskTitle struct {
	Title    string
	Source   claudeTitleSource
	Resolved bool
}

func claudeConfigDirectory() (string, error) {
	if directory := os.Getenv("CLAUDE_CONFIG_DIR"); directory != "" {
		return directory, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".claude"), err
}

func (ledger *UsageLedger) readClaudeRunningSessions(directory string) (map[string]cachedClaudeTaskTitle, error) {
	cache := &ledger.sessionFiles
	cache.Lock()
	defer cache.Unlock()
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		cache.ClaudeRegistry = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	next := make(map[string]cachedClaudeRegistryEntry, len(entries))
	running := make(map[string]cachedClaudeTaskTitle, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fingerprint := fingerprintSessionFile(info)
		previous, known := cache.ClaudeRegistry[path]
		if !known || previous.Fingerprint != fingerprint {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			previous = cachedClaudeRegistryEntry{Fingerprint: fingerprint}
			if json.Unmarshal(data, &previous.Session) != nil {
				// An incomplete entry keeps the last title and is retried next cycle.
				continue
			}
		}
		next[path] = previous
		if previous.Session.SessionID != "" {
			running[previous.Session.SessionID] = cachedClaudeTaskTitle{
				Title:  strings.TrimSpace(previous.Session.Name),
				Source: claudeTitleSource{Path: path, Fingerprint: fingerprint},
			}
		}
	}
	cache.ClaudeRegistry = next
	return running, nil
}

func (ledger *UsageLedger) claudeCodeTaskTitles(sessions map[string]bool) (map[string]string, error) {
	directory, err := claudeConfigDirectory()
	if err != nil {
		return nil, err
	}
	running, err := ledger.readClaudeRunningSessions(filepath.Join(directory, "sessions"))
	if err != nil {
		return nil, err
	}
	titles := make(map[string]string, len(sessions))
	readTranscripts := make(map[string]bool)
	for session := range sessions {
		current, active := running[session]
		previous, known, err := ledger.cachedClaudeTaskTitle(directory, session)
		if err != nil {
			return nil, err
		}
		if known && previous.Title != "" && (!active || previous.Source == current.Source) {
			titles[session] = previous.Title
			continue
		}
		if current.Title == "" {
			readTranscripts[session] = true
			continue
		}
		titles[session] = current.Title
		if err := ledger.rememberClaudeTaskTitle(directory, session, current); err != nil {
			return nil, err
		}
	}
	if len(readTranscripts) == 0 {
		return titles, nil
	}
	paths, err := ledger.cachedTitlePaths(filepath.Join(directory, "projects"), false, readTranscripts)
	if err != nil {
		return nil, err
	}
	for session := range readTranscripts {
		current := running[session]
		for _, path := range paths[session] {
			current.Title, err = ledger.claudeCodeSessionTitle(path, session)
			if err != nil {
				return nil, err
			}
		}
		titles[session] = current.Title
		if current.Title != "" {
			if err := ledger.rememberClaudeTaskTitle(directory, session, current); err != nil {
				return nil, err
			}
		}
	}
	return titles, nil
}

func (ledger *UsageLedger) cachedClaudeTaskTitle(directory, session string) (cachedClaudeTaskTitle, bool, error) {
	cache := &ledger.sessionFiles
	cache.Lock()
	defer cache.Unlock()
	if previous, loaded := cache.ClaudeTitles[session]; loaded {
		return previous, previous.Resolved, nil
	}
	key := filepath.Join(directory, "sessions", session)
	var previous cachedClaudeTaskTitle
	err := ledger.sqlDatabase.QueryRow(`SELECT name FROM session_titles WHERE path=? AND kind='claude-title'`, key).Scan(&previous.Title)
	if err != nil && err != sql.ErrNoRows {
		return previous, false, err
	}
	previous.Resolved = err == nil
	if cache.ClaudeTitles == nil {
		cache.ClaudeTitles = make(map[string]cachedClaudeTaskTitle)
	}
	cache.ClaudeTitles[session] = previous
	return previous, previous.Resolved, nil
}

func (ledger *UsageLedger) rememberClaudeTaskTitle(directory, session string, title cachedClaudeTaskTitle) error {
	cache := &ledger.sessionFiles
	cache.Lock()
	defer cache.Unlock()
	key := filepath.Join(directory, "sessions", session)
	previous := cache.ClaudeTitles[session]
	if !previous.Resolved || previous.Title != title.Title {
		_, err := ledger.sqlDatabase.Exec(`INSERT INTO session_titles VALUES (?,'claude-title','',0,0,0,?,'','','','') ON CONFLICT(path,kind) DO UPDATE SET name=excluded.name`, key, title.Title)
		if err != nil {
			return err
		}
	}
	title.Resolved = true
	cache.ClaudeTitles[session] = title
	return nil
}

// A single directory listing replaces a wildcard expansion for every task.
// Nested matches precede flat Pi matches, preserving the old glob order.
func discoverTitlePathsWithDirectoryReader(directory string, pi bool, sessions map[string]bool, readDirectory func(string) ([]os.DirEntry, error)) (map[string][]string, error) {
	paths := make(map[string][]string)
	entries, err := readDirectory(directory)
	if err != nil {
		return paths, nil // filepath.Glob also ignores directory read errors.
	}
	add := func(path, name string) {
		if !strings.HasSuffix(name, ".jsonl") {
			return
		}
		session := strings.TrimSuffix(name, ".jsonl")
		if !pi {
			if sessions[session] {
				paths[session] = append(paths[session], path)
			}
			return
		}
		for {
			_, suffix, found := strings.Cut(session, "_")
			if !found {
				break
			}
			session = suffix
			if sessions[session] {
				paths[session] = append(paths[session], path)
			}
		}
	}
	for _, entry := range entries {
		// Glob follows directory symlinks too.
		subdirectory := filepath.Join(directory, entry.Name())
		if !entry.IsDir() {
			if entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			info, statErr := os.Stat(subdirectory)
			if statErr != nil || !info.IsDir() {
				continue
			}
		}
		children, readErr := readDirectory(subdirectory)
		if readErr != nil {
			continue
		} // Like filepath.Glob, unreadable directories have no matches.
		for _, child := range children {
			add(filepath.Join(subdirectory, child.Name()), child.Name())
		}
	}
	if pi {
		for _, entry := range entries {
			add(filepath.Join(directory, entry.Name()), entry.Name())
		}
	}
	return paths, nil
}

func (ledger *UsageLedger) claudeCodeSessionTitle(path, sessionID string) (string, error) {
	state, err := ledger.readSessionTitle(path, "claude", func(state *sessionTitleState, line []byte) {
		if !bytes.Contains(line, []byte(`"custom-title"`)) && !bytes.Contains(line, []byte(`"ai-title"`)) && !bytes.Contains(line, []byte(`"summary"`)) && state.FirstPrompt != "" {
			return
		}
		var record struct {
			Type        string `json:"type"`
			SessionID   string `json:"sessionId"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
			Summary     string `json:"summary"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &record) != nil || (record.SessionID != sessionID && record.SessionID != "") {
			return
		}
		switch record.Type {
		case "custom-title":
			state.Name = strings.TrimSpace(record.CustomTitle)
		case "ai-title":
			state.Generated = strings.TrimSpace(record.AITitle)
		case "summary":
			state.Summary = strings.TrimSpace(record.Summary)
		case "user":
			if !record.IsMeta && state.FirstPrompt == "" {
				state.FirstPrompt = claudePromptTitle(record.Message.Content)
			}
		}
	})
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name := state.Name

	override, err := ledger.cachedClaudeCustomTitle(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "custom-title.json"))
	if err != nil {
		return "", err
	}
	if override != "" {
		name = override
	}

	for _, title := range []string{name, state.Generated, state.Summary, state.FirstPrompt} {
		if title != "" {
			return title, nil
		}
	}
	return "", nil
}

// Claude's session picker uses the first prompt when no saved title exists.
func claudePromptTitle(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return ""
		}
		for _, block := range blocks {
			if block.Type == "text" {
				text = block.Text
				break
			}
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	characters := []rune(text)
	if len(characters) > 200 {
		text = string(characters[:200])
	}
	return text
}

func (ledger *UsageLedger) cachedClaudeCustomTitle(path string) (string, error) {
	cache := &ledger.sessionFiles
	cache.Lock()
	defer cache.Unlock()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		delete(cache.CustomTitles, path)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	fingerprint := fingerprintSessionFile(info)
	if previous, exists := cache.CustomTitles[path]; exists && previous.Fingerprint == fingerprint {
		return previous.Name, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var custom struct {
		Title string `json:"customTitle"`
	}
	var name string
	if json.Unmarshal(data, &custom) == nil {
		name = strings.TrimSpace(custom.Title)
	}
	if cache.CustomTitles == nil {
		cache.CustomTitles = make(map[string]cachedCustomTitle)
	}
	cache.CustomTitles[path] = cachedCustomTitle{Fingerprint: fingerprint, Name: name}
	return name, nil
}

// Stat cached directories before using known paths: a new duplicate session
// file can change glob-order title precedence. A directory change makes its
// path sets unknown; only changed directories are listed again. Child changes
// matter independently of the projects directory.
func (ledger *UsageLedger) cachedTitlePaths(directory string, pi bool, sessions map[string]bool) (map[string][]string, error) {
	cache := &ledger.sessionFiles
	cache.Lock()
	defer cache.Unlock()
	key := directory
	if pi {
		key = "pi\x00" + directory
	}
	if cache.TitlePaths == nil {
		cache.TitlePaths = make(map[string]map[string][]string)
	}
	known := cache.TitlePaths[key]
	for path, previous := range cache.Directories {
		if path != directory && filepath.Dir(path) != directory {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || fingerprintSessionFile(info) != previous.Fingerprint {
			known = nil
			if err != nil {
				delete(cache.Directories, path)
			}
		}
	}
	if known == nil {
		known = make(map[string][]string)
		cache.TitlePaths[key] = known
	}
	paths := make(map[string][]string, len(sessions))
	missing := make(map[string]bool)
	for session := range sessions {
		previous := known[session]
		if len(previous) != 0 {
			paths[session] = previous
		} else {
			missing[session] = true
		}
	}
	if len(missing) == 0 {
		return paths, nil
	}
	found, err := discoverTitlePathsWithDirectoryReader(directory, pi, missing, func(path string) ([]os.DirEntry, error) {
		info, err := os.Stat(path)
		if err != nil {
			delete(cache.Directories, path)
			return nil, err
		}
		fingerprint := fingerprintSessionFile(info)
		if previous, exists := cache.Directories[path]; exists && previous.Fingerprint == fingerprint {
			return previous.Entries, nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		if cache.Directories == nil {
			cache.Directories = make(map[string]cachedTitleDirectory)
		}
		cache.Directories[path] = cachedTitleDirectory{Fingerprint: fingerprint, Entries: entries}
		return entries, nil
	})
	if err != nil {
		return nil, err
	}
	for session := range missing {
		known[session] = found[session]
		paths[session] = found[session]
	}
	return paths, nil
}

// Refresh Pi title lookup only for sessions not covered by the last walk.
// The existing lookup retains its filename ordering and symlink handling.
func (ledger *UsageLedger) discoveredPiTitlePaths(sessions map[string]bool) (map[string][]string, error) {
	state := &ledger.piDiscovery
	missing := make(map[string]bool)
	for session := range sessions {
		if _, known := state.TitlePaths[session]; !known {
			missing[session] = true
		}
	}
	if len(missing) > 0 {
		found, err := ledger.cachedTitlePaths(state.Directory, true, missing)
		if err != nil {
			return nil, err
		}
		for session := range missing {
			state.TitlePaths[session] = found[session]
		}
	}
	return state.TitlePaths, nil
}
