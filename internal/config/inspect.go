package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// LocalKeySource contains safe metadata about a local API key declaration.
type LocalKeySource struct {
	Name      string
	Variable  string
	Dashboard bool
	Line      int
	Literal   bool
}

// InspectLocalKeySources returns key names, variable references, access and source lines without key values.
func InspectLocalKeySources(path string) ([]LocalKeySource, error) {
	doc, err := OpenDocument(path)
	if err != nil {
		return nil, err
	}
	root, _, err := parseDocument(doc.body())
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	server := mappingValue(root.Content[0], "server")
	keys := mappingValue(server, "api_keys")
	if keys == nil || keys.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config: server.api_keys must be a list")
	}
	result := make([]LocalKeySource, 0, len(keys.Content))
	for index, entry := range keys.Content {
		source := LocalKeySource{Name: fmt.Sprintf("key-%d", index+1), Line: entry.Line, Literal: true}
		var key *yaml.Node
		if entry.Kind == yaml.ScalarNode {
			key = entry
		} else if entry.Kind == yaml.MappingNode {
			if name := mappingValue(entry, "name"); name != nil {
				source.Name = name.Value
			}
			key = mappingValue(entry, "key")
			if dashboard := mappingValue(entry, "dashboard"); dashboard != nil {
				source.Dashboard = dashboard.Value == "true"
			}
		}
		if key == nil {
			return nil, fmt.Errorf("config: server.api_keys entry %d has no key", index+1)
		}
		source.Line = key.Line
		if providerKeyReference.MatchString(key.Value) {
			source.Variable = key.Value[2 : len(key.Value)-1]
			source.Literal = false
		}
		result = append(result, source)
	}
	return result, nil
}
