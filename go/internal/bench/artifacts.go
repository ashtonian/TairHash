package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// artifacts stores one run's raw captures. The zero value discards them.
type artifacts struct {
	dir string
}

func newArtifacts(root, label string) (artifacts, error) {
	if root == "" {
		return artifacts{}, nil
	}
	dir := filepath.Join(root, strings.ReplaceAll(label, "/", "_"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return artifacts{}, fmt.Errorf("create artifacts dir: %w", err)
	}
	return artifacts{dir: dir}, nil
}

func (a artifacts) enabled() bool { return a.dir != "" }

func (a artifacts) path(name string) string { return filepath.Join(a.dir, name) }

func (a artifacts) write(name, content string) error {
	if !a.enabled() {
		return nil
	}
	if err := os.WriteFile(a.path(name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write artifact %s: %w", name, err)
	}
	return nil
}

func (a artifacts) writeJSON(name string, v any) error {
	if !a.enabled() {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact %s: %w", name, err)
	}
	return a.write(name, string(b)+"\n")
}

func writeJSONLines[T any](a artifacts, name string, rows []T) error {
	if !a.enabled() {
		return nil
	}
	var b strings.Builder
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("encode artifact %s: %w", name, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return a.write(name, b.String())
}
