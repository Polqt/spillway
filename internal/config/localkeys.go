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
		path := "server.api_keys[" + strconv.Itoa(i) + "]"
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
		if !isString(value) {
			return LocalKey{}, fmt.Errorf("%s.%s (line %d): must be a string", path, field.Value, value.Line)
		}
		switch field.Value {
		case "name":
			k.Name, hasName = value.Value, true
		case "key":
			k.Key, hasKey = redact.NewSecret(value.Value), true
		default:
			return LocalKey{}, fmt.Errorf("%s.%s (line %d): unknown field; use name and key", path, field.Value, field.Line)
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

// isString reports whether n is a non-null scalar.
func isString(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag != "!!null"
}
