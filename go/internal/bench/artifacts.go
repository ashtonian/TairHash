package bench

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
)

// artifacts stores one run's raw captures. The zero value discards them.
type artifacts struct {
	dir string
}

// newArtifacts creates root/<label dir>/<run>. The label is free text, so it is
// mapped to one path-safe segment; run is a RunID and already path-safe.
func newArtifacts(root, label, run string) (artifacts, error) {
	if root == "" {
		return artifacts{}, nil
	}
	dir := filepath.Join(root, run)
	if label != "" {
		dir = filepath.Join(root, labelDir(label), run)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return artifacts{}, fmt.Errorf("create artifacts dir: %w", err)
	}
	return artifacts{dir: dir}, nil
}

// labelDir maps a label to a single path segment of [A-Za-z0-9._@+-]. When that
// changes the label, a hash of the original is appended so distinct labels
// never share a directory.
func labelDir(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range label {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@+-", r)
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-.")
	if slug == label {
		return slug
	}
	h := fnv.New32a()
	h.Write([]byte(label))
	if slug == "" {
		return fmt.Sprintf("label-%08x", h.Sum32())
	}
	return fmt.Sprintf("%s-%08x", slug, h.Sum32())
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
