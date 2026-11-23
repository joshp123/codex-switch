package codexstate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// TaskTitles names the requested tasks from the current Codex home. A task
// takes the name Codex gave it. A task Codex did not name, such as a codex exec
// run, is named by how it was started and its project, for example
// "codex exec · codex-switch". Codex's stored title is the prompt's first line
// for those runs, so it is never shown.
func (r LocalReader) TaskTitles(ctx context.Context, taskIDs []string) (map[string]string, error) {
	result := map[string]string{}
	if len(taskIDs) == 0 {
		return result, nil
	}
	databasePath, err := r.taskTitleDatabase()
	if err != nil || databasePath == "" {
		return result, err
	}
	databaseURL := url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return nil, err
	}
	defer database.Close()
	const batchSize = 500
	for start := 0; start < len(taskIDs); start += batchSize {
		batch := taskIDs[start:min(start+batchSize, len(taskIDs))]
		parameters := make([]any, len(batch))
		for index, id := range batch {
			parameters[index] = id
		}
		rows, err := database.QueryContext(ctx, "SELECT id,COALESCE(name,''),source,cwd,COALESCE(git_origin_url,'') FROM threads WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+")", parameters...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name, source, directory, origin string
			if err := rows.Scan(&id, &name, &source, &directory, &origin); err != nil {
				rows.Close()
				return nil, err
			}
			result[id] = taskTitle(name, source, directory, origin)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return result, nil
}

// TaskTitleInputs identifies Codex's thread database and its write-ahead log,
// where a new or renamed task lands first. Titles can change only when this
// does.
func (r LocalReader) TaskTitleInputs() (string, error) {
	databasePath, err := r.taskTitleDatabase()
	if err != nil || databasePath == "" {
		return "", err
	}
	return fileIdentity(databasePath) + "\n" + fileIdentity(databasePath+"-wal"), nil
}

// taskTitleDatabase is the newest state database in the Codex home, or ""
// when there is none.
func (r LocalReader) taskTitleDatabase() (string, error) {
	paths, err := filepath.Glob(filepath.Join(r.CodexHome, "state_*.sqlite"))
	if err != nil {
		return "", err
	}
	var newest os.FileInfo
	var databasePath string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if newest == nil || info.ModTime().After(newest.ModTime()) {
			newest = info
			databasePath = path
		}
	}
	return databasePath, nil
}

func taskTitle(name, source, directory, origin string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	launcher := map[string]string{"exec": "codex exec", "cli": "codex"}[source]
	if launcher == "" {
		launcher = "Codex"
	}
	project := strings.TrimSuffix(path.Base(strings.TrimRight(origin, "/")), ".git")
	if origin == "" || project == "." || project == "/" {
		project = filepath.Base(filepath.Clean(directory))
	}
	if project == "" || project == "." || project == string(filepath.Separator) {
		return launcher
	}
	return launcher + " · " + project
}
