package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

func (e *Executor) fileRead(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: map[string]any{"path": req.Path, "content": string(b)}}, nil
}

func (e *Executor) fileWrite(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(full, []byte(req.Content), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Output: map[string]any{"path": req.Path, "bytes": len(req.Content)}}, nil
}

func (e *Executor) fileSearch(ctx context.Context, runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	args := []string{"-n", req.Pattern, "."}
	if req.Glob != "" {
		args = []string{"-n", "--glob", req.Glob, req.Pattern, "."}
	}
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = runCtx.Workspace
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return Result{}, err
	}
	return Result{Output: map[string]any{"matches": string(out)}}, nil
}

func (e *Executor) applyPatch(ctx context.Context, runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	patchFile := filepath.Join(e.ArtifactsDir, runCtx.RunID, "latest.patch")
	if err := os.MkdirAll(filepath.Dir(patchFile), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(patchFile, []byte(req.Patch), 0o644); err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, "bash", "-lc", "git apply --whitespace=nowarn \""+patchFile+"\"")
	cmd.Dir = runCtx.Workspace
	out, err := cmd.CombinedOutput()
	_ = e.insertArtifactRecord(runCtx, "patch", patchFile, "text/x-diff", int64(len(req.Patch)))
	return Result{Output: map[string]any{"applied": err == nil}, Log: string(out), Artifacts: []string{patchFile}}, err
}

func (e *Executor) fileReadRange(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	if req.StartLine <= 0 || req.EndLine <= 0 || req.EndLine < req.StartLine {
		return Result{}, fmt.Errorf("invalid line range")
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return Result{}, err
	}
	lines := strings.Split(string(b), "\n")
	if req.StartLine > len(lines) {
		return Result{Output: map[string]any{"path": req.Path, "content": "", "start_line": req.StartLine, "end_line": req.EndLine}}, nil
	}
	end := req.EndLine
	if end > len(lines) {
		end = len(lines)
	}
	chunk := strings.Join(lines[req.StartLine-1:end], "\n")
	return Result{Output: map[string]any{
		"path":       req.Path,
		"start_line": req.StartLine,
		"end_line":   end,
		"content":    chunk,
	}}, nil
}

func (e *Executor) fileExists(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	_, err = os.Stat(full)
	if err == nil {
		return Result{Output: map[string]any{"path": req.Path, "exists": true}}, nil
	}
	if os.IsNotExist(err) {
		return Result{Output: map[string]any{"path": req.Path, "exists": false}}, nil
	}
	return Result{}, err
}

func (e *Executor) fileStat(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: map[string]any{
		"path":      req.Path,
		"size":      info.Size(),
		"is_dir":    info.IsDir(),
		"mode":      info.Mode().String(),
		"modified":  info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"basename":  info.Name(),
		"extension": filepath.Ext(info.Name()),
	}}, nil
}

func (e *Executor) fileList(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path       string `json:"path"`
		Recursive  bool   `json:"recursive"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	if req.Path == "" {
		req.Path = "."
	}
	if req.MaxEntries <= 0 {
		req.MaxEntries = 200
	}
	root, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	entries := make([]map[string]any, 0)
	if req.Recursive {
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if len(entries) >= req.MaxEntries {
				return fs.SkipAll
			}
			rel, _ := filepath.Rel(runCtx.Workspace, p)
			entries = append(entries, map[string]any{"path": filepath.ToSlash(rel), "is_dir": d.IsDir()})
			return nil
		})
		if err != nil && err != fs.SkipAll {
			return Result{}, err
		}
	} else {
		dirEntries, err := os.ReadDir(root)
		if err != nil {
			return Result{}, err
		}
		for i, d := range dirEntries {
			if i >= req.MaxEntries {
				break
			}
			p := filepath.Join(root, d.Name())
			rel, _ := filepath.Rel(runCtx.Workspace, p)
			entries = append(entries, map[string]any{"path": filepath.ToSlash(rel), "is_dir": d.IsDir()})
		}
	}
	return Result{Output: map[string]any{"entries": entries, "count": len(entries)}}, nil
}

func (e *Executor) filePublish(ctx context.Context, runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	full, err := safePath(runCtx.Workspace, req.Path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return Result{}, fmt.Errorf("file not found: %s", req.Path)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("path is a directory, not a file")
	}
	mime := guessMIMEFromPath(full)
	kind := guessArtifactKind(full)
	if err := e.insertArtifactRecord(runCtx, kind, full, mime, info.Size()); err != nil {
		return Result{}, err
	}
	var artifactID string
	if e.DB != nil {
		_ = e.DB.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE run_id=? AND path=? ORDER BY created_at DESC LIMIT 1`, runCtx.RunID, full).Scan(&artifactID)
	}
	return Result{Output: map[string]any{
		"ok":            true,
		"artifact_id":   artifactID,
		"path":          req.Path,
		"download_path": "/api/runs/" + runCtx.RunID + "/artifacts/" + artifactID + "/content",
	}}, nil
}

func (e *Executor) pathGlob(runCtx Context, input json.RawMessage) (Result, error) {
	var req struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Result{}, err
	}
	pattern := strings.TrimSpace(req.Pattern)
	if pattern == "" {
		return Result{}, fmt.Errorf("pattern required")
	}
	matches := make([]string, 0)
	_ = filepath.WalkDir(runCtx.Workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(runCtx.Workspace, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		ok, _ := path.Match(pattern, rel)
		if ok {
			matches = append(matches, rel)
		}
		return nil
	})
	return Result{Output: map[string]any{"pattern": pattern, "matches": matches, "count": len(matches)}}, nil
}
