package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ValidateAndSwap validates candidate as a full config (the same code path as
// boot), backs the current file up to .bak, and atomically replaces it.
//
// It is the one writer shared by the console's config/model editors and the
// Studio workspace: a candidate that would not boot never reaches the file.
func ValidateAndSwap(path string, candidate []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agentflow-*.yaml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(candidate); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if _, err := Load(tmpName); err != nil {
		return fmt.Errorf("result does not validate: %w", err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	if err := os.WriteFile(path+".bak", cur, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Windows rename-over-existing can fail on some filesystems; the
		// backup already exists, so a direct write is a safe fallback.
		if err2 := os.WriteFile(path, candidate, 0o600); err2 != nil {
			return fmt.Errorf("replace config: %w (fallback: %v)", err, err2)
		}
	}
	return nil
}

// MappingRoot returns the root mapping node of a YAML document.
func MappingRoot(doc *yaml.Node) (*yaml.Node, error) {
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root is not a mapping")
	}
	return doc.Content[0], nil
}

// SetMapKey replaces key's value in a mapping node, or appends it. Existing
// comments and the order of other keys are preserved — the Studio writes one
// agent without reformatting a hand-maintained config.
func SetMapKey(root *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = value
			return
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		value)
}

// DeleteMapKey removes key from a mapping node, reporting whether it was
// present.
func DeleteMapKey(root *yaml.Node, key string) bool {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			return true
		}
	}
	return false
}

// MapValue returns key's value node in a mapping, or nil.
func MapValue(root *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

// MarshalDocument renders a YAML document node with the deployment's
// two-space indentation.
func MarshalDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
