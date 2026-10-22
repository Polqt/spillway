package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/Polqt/spillway/internal/redact"
)

// LocalKey is one local key and the name of the client app that presents it.
type LocalKey struct {
	Name      string        `yaml:"name"`                // Name of the client app.
	Key       redact.Secret `yaml:"key"`                 // Local bearer key for the client app.
	Dashboard bool          `yaml:"dashboard,omitempty"` // Grants read-only dashboard API access.
}

// LocalKeys is the server.api_keys list. Each entry is either a plain string,
// named key-N by its 1-based list position, or a {name, key} mapping.
type LocalKeys []LocalKey

// LoadLocalKeys reads named local keys without resolving or validating providers.
func LoadLocalKeys(path string) (LocalKeys, error) {
	return LoadLocalKeysWithEnv(path, os.LookupEnv)
}

// LoadLocalKeysWithEnv reads local keys and expands their environment references.
func LoadLocalKeysWithEnv(path string, lookupEnv func(string) (string, bool)) (LocalKeys, error) {
	root, err := readConfigDocument(path)
	if err != nil {
		return nil, err
	}
	server := mappingValue(root, "server")
	keyNode := mappingValue(server, "api_keys")
	if keyNode == nil {
		return nil, errors.New("config: no local keys found in server.api_keys")
	}
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	for i, entry := range keyNode.Content {
		path := localKeyPath(i)
		if entry.Kind == yaml.ScalarNode {
			if err := expandScalar(entry, path, lookupEnv); err != nil {
				return nil, fmt.Errorf("config: %w", err)
			}
			continue
		}
		for _, field := range []struct{ name, path string }{{"name", path + ".name"}, {"key", path + ".key"}} {
			if value := mappingValue(entry, field.name); value != nil {
				if err := expandScalar(value, field.path, lookupEnv); err != nil {
					return nil, fmt.Errorf("config: %w", err)
				}
			}
		}
	}
	var keys LocalKeys
	if err := keyNode.Decode(&keys); err != nil {
		return nil, errors.New("config: invalid server.api_keys")
	}
	if len(keys) == 0 {
		return nil, errors.New("config: no local keys found in server.api_keys")
	}
	return keys, nil
}

// UnmarshalYAML decodes both entry forms. It is defined on the slice because a
// generated name needs the entry's position. Errors name the YAML path and
// line, never a key value.
func (ks *LocalKeys) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("server.api_keys (line %d): must be a list of local keys", n.Line)
	}
	keys := make(LocalKeys, 0, len(n.Content))
	for i, entry := range n.Content {
		path := localKeyPath(i)
		switch {
		case isString(entry):
			keys = append(keys, LocalKey{Name: "key-" + strconv.Itoa(i+1), Key: redact.NewSecret(entry.Value)})
		case entry.Kind == yaml.MappingNode:
			k, err := decodeNamedKey(entry, path)
			if err != nil {
				return err
			}
			keys = append(keys, k)
		default:
			return fmt.Errorf("%s (line %d): must be a string or a mapping with name and key", path, entry.Line)
		}
	}
	*ks = keys
	return nil
}

// decodeNamedKey reads a {name, key} mapping field by field, because
// node.Decode would not inherit the decoder's KnownFields setting.
func decodeNamedKey(n *yaml.Node, path string) (LocalKey, error) {
	var k LocalKey
	var hasName, hasKey bool
	for j := 0; j+1 < len(n.Content); j += 2 {
		field, value := n.Content[j], n.Content[j+1]
		if field.Value != "name" && field.Value != "key" && field.Value != "dashboard" {
			// The field is not printed: a local key mistyped as a mapping
			// key ("- sk-...:") would otherwise leak into the error.
			return LocalKey{}, fmt.Errorf("%s (line %d): unknown field; an entry takes only name, key and dashboard", path, field.Line)
		}
		if field.Value == "dashboard" {
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return LocalKey{}, fmt.Errorf("%s.dashboard (line %d): must be true or false", path, value.Line)
			}
			k.Dashboard = value.Value == "true"
			continue
		}
		if !isString(value) {
			return LocalKey{}, fmt.Errorf("%s.%s (line %d): must be a string", path, field.Value, value.Line)
		}
		if field.Value == "name" {
			k.Name, hasName = value.Value, true
		} else {
			k.Key, hasKey = redact.NewSecret(value.Value), true
		}
	}
	if !hasName {
		return LocalKey{}, fmt.Errorf("%s.name is required (line %d)", path, n.Line)
	}
	if !hasKey {
		return LocalKey{}, fmt.Errorf("%s.key is required (line %d)", path, n.Line)
	}
	return k, nil
}

// LoadListenWithEnv reads server.listen without resolving or validating
// providers, so local tools can find the gateway a config describes.
func LoadListenWithEnv(path string, lookupEnv func(string) (string, bool)) (string, error) {
	root, err := readConfigDocument(path)
	if err != nil {
		return "", err
	}
	listen := mappingValue(mappingValue(root, "server"), "listen")
	if listen == nil {
		return defaultListen, nil
	}
	if listen.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("config: server.listen (line %d): must be a string", listen.Line)
	}
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	if err := expandScalar(listen, "server.listen", lookupEnv); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return listen.Value, nil
}

// readConfigDocument returns the root node of a size-limited config file.
func readConfigDocument(path string) (*yaml.Node, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is the operator's explicit -config flag.
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("config: %s is larger than 1 MiB", path)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil, errors.New("config: invalid YAML")
	}
	return doc.Content[0], nil
}

// localKeyPath is the YAML path of the i-th server.api_keys entry, for errors.
func localKeyPath(i int) string {
	return "server.api_keys[" + strconv.Itoa(i) + "]"
}

// isString reports whether n is a string scalar.
func isString(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!str"
}
