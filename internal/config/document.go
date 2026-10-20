package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// Document is a config file read for editing. It keeps the exact bytes so an
// edit changes only the lines it touches.
type Document struct {
	path    string
	data    []byte
	digest  [32]byte
	mode    os.FileMode
	newline string
}

// OpenDocument reads path, following symlinks to the real file, and parses it
// without expanding environment references.
func OpenDocument(path string) (*Document, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	f, err := os.Open(resolved) // #nosec G304 -- the path is the operator's own -config flag
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", resolved, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("config: %s is larger than 1 MiB", resolved)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("config: stat %s: %w", resolved, err)
	}
	doc := &Document{path: resolved, data: data, digest: sha256.Sum256(data), mode: info.Mode().Perm(), newline: "\n"}
	if bytes.Contains(data, []byte("\r\n")) {
		doc.newline = "\r\n"
	}
	if _, _, err := parseDocument(doc.body()); err != nil {
		return nil, fmt.Errorf("config: %s: %w", resolved, err)
	}
	return doc, nil
}

// Path returns the resolved absolute path that Commit replaces.
func (d *Document) Path() string {
	return d.path
}

// Edit returns an empty ordered edit of d.
func (d *Document) Edit() *Edit {
	return &Edit{doc: d}
}

// ProviderKeyVariable returns the environment-variable name used by one provider.
func ProviderKeyVariable(path, provider string) (string, error) {
	doc, err := OpenDocument(path)
	if err != nil {
		return "", err
	}
	root, _, err := parseDocument(doc.body())
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	entry := mappingValue(mappingValue(root.Content[0], "providers"), provider)
	key := mappingValue(entry, "api_key")
	if key == nil {
		return "", nil
	}
	if !providerKeyReference.MatchString(key.Value) {
		return "", fmt.Errorf("config: providers.%s.api_key is not an environment-variable reference", provider)
	}
	return strings.TrimSuffix(strings.TrimPrefix(key.Value, "${"), "}"), nil
}

// body returns the file without a UTF-8 byte order mark, which the YAML
// positions are relative to; the mark is restored on output.
func (d *Document) body() []byte {
	return bytes.TrimPrefix(d.data, utf8BOM)
}

func (d *Document) bom() string {
	if bytes.HasPrefix(d.data, utf8BOM) {
		return string(utf8BOM)
	}
	return ""
}

// parseDocument decodes the first YAML document and reports whether another follows.
func parseDocument(body []byte) (*yaml.Node, bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, errors.New("config is empty")
		}
		return nil, false, fmt.Errorf("invalid YAML: %w", err)
	}
	var next yaml.Node
	err := dec.Decode(&next)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("invalid YAML: %w", err)
	}
	return &root, err == nil, nil
}

// splitLines splits body into lines that keep their terminators, so joining
// them reproduces body exactly.
func splitLines(body []byte) []string {
	text := string(body)
	var lines []string
	for text != "" {
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:end+1])
		text = text[end+1:]
	}
	return lines
}

func lineText(line string) string {
	return strings.TrimRight(line, "\r\n")
}

func isBlankOrComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}
