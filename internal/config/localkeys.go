package config

import (
	"fmt"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/Polqt/spillway/internal/redact"
)

// LocalKey is one local key and the name of the client app that presents it.
type LocalKey struct {
	Name string        `yaml:"name"`
	Key  redact.Secret `yaml:"key"`
}

// LocalKeys is the server.api_keys list. Each entry is either a plain string,
// named key-N by its 1-based list position, or a {name, key} mapping.
type LocalKeys []LocalKey

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
		if field.Value != "name" && field.Value != "key" {
			// The field is not printed: a local key mistyped as a mapping
			// key ("- sk-...:") would otherwise leak into the error.
			return LocalKey{}, fmt.Errorf("%s (line %d): unknown field; an entry takes only name and key", path, field.Line)
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

// localKeyPath is the YAML path of the i-th server.api_keys entry, for errors.
func localKeyPath(i int) string {
	return "server.api_keys[" + strconv.Itoa(i) + "]"
}

// isString reports whether n is a non-null scalar.
func isString(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag != "!!null"
}
