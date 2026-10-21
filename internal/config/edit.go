package config

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
	"go.yaml.in/yaml/v3"
)

// modelReferencePattern keeps written model references plain YAML scalars.
var modelReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

// Edit is an ordered set of changes to one Document.
type Edit struct {
	doc *Document
	ops []editOp
}

// editOp is one change: apply splices lines into the file, and intend makes
// the same change to a generic decoded tree so Result can prove nothing else moved.
type editOp interface {
	apply(root *yaml.Node, lines []string, newline string) ([]string, hunk, error)
	intend(tree map[string]any) error
	// layout refuses the file layout with the block-style lines to add by hand.
	layout(line int, reason string) *LayoutError
}

type hunk struct {
	path    string
	context []string
	added   []string
}

// ProviderEntry is a provider to add. It has no key field, so a key value
// cannot be written through it.
type ProviderEntry struct {
	Name     string // Name is the provider preset name.
	KeyEnv   string // KeyEnv is the variable name written as "${KeyEnv}"; empty for presets without keys.
	BaseURL  string // BaseURL overrides the preset URL; empty keeps the preset default.
	FreeTier bool   // FreeTier enables free-tier attestation for Groq or Gemini.
}

// ProviderExistsError reports that the provider is already configured.
// Spillway keeps one entry per provider (no account rotation), so it is never merged.
type ProviderExistsError struct {
	Name string // Name is the provider already present in the configuration.
	Line int    // Line is the source line of the existing provider.
}

func (e *ProviderExistsError) Error() string {
	return fmt.Sprintf("config: providers.%s (line %d): %s is already configured", e.Name, e.Line, e.Name)
}

// LayoutError reports a file layout that an edit will not rewrite, with the
// YAML snippet the user can add by hand.
type LayoutError struct {
	Path    string // Path is the YAML path such as tiers.free.models.
	Line    int    // Line is the source line of the unsupported layout.
	Reason  string // Reason explains why the layout cannot be edited safely.
	Snippet string // Snippet is YAML the user can add manually.
}

func (e *LayoutError) Error() string {
	location := e.Path
	if e.Line > 0 {
		location = fmt.Sprintf("%s (line %d)", e.Path, e.Line)
	}
	return fmt.Sprintf("config: %s: %s; add this by hand:\n%s", location, e.Reason, e.Snippet)
}

// AddProvider adds one preset provider to the providers mapping.
func (e *Edit) AddProvider(p ProviderEntry) error {
	preset, ok := providers.LookupPreset(p.Name)
	if !ok {
		return fmt.Errorf("config: %q is not a supported provider; use one of %s", p.Name, strings.Join(knownProviders(), ", "))
	}
	switch {
	case p.KeyEnv != "" && (!environmentName.MatchString(p.KeyEnv) || looksLikeKey(p.KeyEnv)):
		return errors.New("config: the key variable must be an environment variable name such as GEMINI_API_KEY, not a key value")
	case p.KeyEnv != "" && !preset.AcceptsKey:
		return fmt.Errorf("config: %s does not take a provider key", p.Name)
	case p.KeyEnv == "" && preset.RequiresKey:
		return fmt.Errorf("config: %s needs a key variable such as %s", p.Name, preset.KeyEnv)
	case p.FreeTier && !preset.FreeTierAttestable:
		return fmt.Errorf("config: free_tier is supported only for groq and gemini, not %s", p.Name)
	case strings.ContainsAny(p.BaseURL, "\"\\") || strings.ContainsFunc(p.BaseURL, isControl):
		return errors.New("config: the base URL contains a quote, backslash or control character")
	}
	root, _, err := parseDocument(e.doc.body())
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := providerAbsent(root.Content[0], p.Name); err != nil {
		return err
	}
	e.ops = append(e.ops, addProviderOp{entry: p})
	return nil
}

// AppendTierModel appends a model reference to an existing tier's models list.
func (e *Edit) AppendTierModel(tier, modelRef string) error {
	if !configName.MatchString(tier) {
		return fmt.Errorf("config: %q is not a valid tier name", tier)
	}
	if !modelReferencePattern.MatchString(modelRef) || !strings.Contains(modelRef, "/") {
		return fmt.Errorf("config: %q is not a model reference such as gemini/gemini-2.5-flash", modelRef)
	}
	e.ops = append(e.ops, appendTierModelOp{tier: tier, model: modelRef})
	return nil
}

// ReplaceModelReference replaces one exact model reference wherever config accepts one.
func (e *Edit) ReplaceModelReference(oldRef, newRef string) error {
	for _, ref := range []string{oldRef, newRef} {
		if !modelReferencePattern.MatchString(ref) || !strings.Contains(ref, "/") {
			return fmt.Errorf("config: %q is not a model reference such as gemini/gemini-2.5-flash", ref)
		}
	}
	if oldRef == newRef {
		return errors.New("config: old and new model references are the same")
	}
	e.ops = append(e.ops, replaceModelReferenceOp{old: oldRef, new: newRef})
	return nil
}

// looksLikeKey catches a provider key pasted where a variable name belongs;
// Groq and Gemini keys are otherwise valid variable names.
func looksLikeKey(name string) bool {
	return len(name) > 64 || strings.Contains(name, "sk-") || strings.Contains(name, "gsk_") || strings.Contains(name, "AIza")
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// Result applies the edit, validates the new file with the normal parser and
// a unique placeholder per referenced variable, and proves that only the
// intended values changed.
func (e *Edit) Result() (Change, error) {
	before := e.doc.body()
	lines := splitLines(before)
	hunks := make([]hunk, 0, len(e.ops))
	for _, op := range e.ops {
		root, multiple, err := parseDocument([]byte(strings.Join(lines, "")))
		if err != nil {
			return Change{}, fmt.Errorf("config: %w", err)
		}
		top := root.Content[0]
		switch {
		case multiple:
			return Change{}, op.layout(0, "the file holds more than one YAML document")
		case usesAnchors(root):
			return Change{}, op.layout(0, "the file uses YAML anchors, aliases or merge keys, which a line edit cannot keep consistent")
		case !isBlockMapping(top):
			return Change{}, op.layout(top.Line, "the file is not a block-style YAML mapping")
		}
		next, h, err := op.apply(top, lines, e.doc.newline)
		if err != nil {
			return Change{}, err
		}
		lines = next
		hunks = append(hunks, h)
	}
	after := []byte(e.doc.bom() + strings.Join(lines, ""))
	placeholder := func(name string) (string, bool) { return "placeholder-" + name, true }
	if _, err := ParseWithEnv(after, placeholder); err != nil {
		return Change{}, fmt.Errorf("edited config would be invalid: %w", err)
	}
	if err := e.proveIntent(before, []byte(strings.Join(lines, ""))); err != nil {
		return Change{}, err
	}
	root, _, err := parseDocument([]byte(strings.Join(lines, "")))
	if err != nil {
		return Change{}, fmt.Errorf("config: %w", err)
	}
	return Change{after: after, beforePath: e.doc.path, beforeDigest: e.doc.digest, hunks: hunks, secrets: localKeyLiterals(root.Content[0])}, nil
}

func (e *Edit) proveIntent(before, after []byte) error {
	var want, got map[string]any
	if err := yaml.Unmarshal(before, &want); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := yaml.Unmarshal(after, &got); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	for _, op := range e.ops {
		if err := op.intend(want); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(want, got) {
		return errors.New("config: internal error: the edit changed more than intended; nothing was written")
	}
	return nil
}

// Change is a validated edit ready to preview or commit.
type Change struct {
	after        []byte
	beforePath   string
	beforeDigest [32]byte
	hunks        []hunk
	secrets      []redact.Secret
}

// Content returns the edited file.
func (c Change) Content() []byte {
	return append([]byte(nil), c.after...)
}

// WritePreview writes a unified-diff style preview with local key values redacted.
func (c Change) WritePreview(w io.Writer) error {
	scrubber := redact.NewScrubber(c.secrets...)
	var out strings.Builder
	for _, h := range c.hunks {
		fmt.Fprintf(&out, "@@ %s @@\n", h.path)
		for _, line := range h.context {
			fmt.Fprintf(&out, " %s\n", scrubber.Scrub(line))
		}
		for _, line := range h.added {
			fmt.Fprintf(&out, "+%s\n", scrubber.Scrub(line))
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

type addProviderOp struct {
	entry ProviderEntry
}

func (op addProviderOp) apply(root *yaml.Node, lines []string, newline string) ([]string, hunk, error) {
	if err := providerAbsent(root, op.entry.Name); err != nil {
		return nil, hunk{}, err
	}
	index := mappingIndex(root, "providers")
	if index < 0 {
		return nil, hunk{}, op.layout(0, "the file has no providers section")
	}
	section := root.Content[index+1]
	if !isBlockMapping(section) {
		return nil, hunk{}, op.layout(section.Line, "providers is written in flow style or is empty")
	}
	indent := strings.Repeat(" ", section.Content[0].Column-1)
	step := childIndent(section)
	added := op.lines(indent, step)
	next := len(lines) + 1
	if index+2 < len(root.Content) {
		next = root.Content[index+2].Line
	}
	return splice(lines, lastContentLine(lines, root.Content[index].Line, next), added, newline, "providers")
}

func (op addProviderOp) lines(indent, step string) []string {
	p := op.entry
	var fields []string
	if p.KeyEnv != "" {
		fields = append(fields, indent+step+`api_key: "${`+p.KeyEnv+`}"`)
	}
	if p.BaseURL != "" {
		fields = append(fields, indent+step+`base_url: "`+p.BaseURL+`"`)
	}
	if p.FreeTier {
		fields = append(fields, indent+step+"free_tier: true")
	}
	if len(fields) == 0 {
		return []string{indent + p.Name + ": {}"}
	}
	return append([]string{indent + p.Name + ":"}, fields...)
}

func (op addProviderOp) layout(line int, reason string) *LayoutError {
	snippet := "providers:\n" + strings.Join(op.lines("  ", "  "), "\n") + "\n"
	return &LayoutError{Path: "providers", Line: line, Reason: reason, Snippet: snippet}
}

func (op addProviderOp) intend(tree map[string]any) error {
	section, ok := tree["providers"].(map[string]any)
	if !ok {
		return errors.New("config: internal error: providers is not a mapping")
	}
	entry := map[string]any{}
	if op.entry.KeyEnv != "" {
		entry["api_key"] = "${" + op.entry.KeyEnv + "}"
	}
	if op.entry.BaseURL != "" {
		entry["base_url"] = op.entry.BaseURL
	}
	if op.entry.FreeTier {
		entry["free_tier"] = true
	}
	section[op.entry.Name] = entry
	return nil
}

type appendTierModelOp struct {
	tier, model string
}

type replaceModelReferenceOp struct{ old, new string }

func (op replaceModelReferenceOp) apply(root *yaml.Node, lines []string, newline string) ([]string, hunk, error) {
	var matches []*yaml.Node
	add := func(node *yaml.Node) {
		if node != nil && node.Kind == yaml.ScalarNode && node.Value == op.old {
			matches = append(matches, node)
		}
	}
	top := root
	if tiers := mappingValue(top, "tiers"); tiers != nil && tiers.Kind == yaml.MappingNode {
		for i := 1; i < len(tiers.Content); i += 2 {
			if models := mappingValue(tiers.Content[i], "models"); models != nil && models.Kind == yaml.SequenceNode {
				foundOld, foundNew := false, false
				for _, item := range models.Content {
					foundOld = foundOld || item.Value == op.old
					foundNew = foundNew || item.Value == op.new
					add(item)
				}
				if foundOld && foundNew {
					return nil, hunk{}, fmt.Errorf("config: tier already contains %s", op.new)
				}
			}
		}
	}
	for _, sectionName := range []string{"models", "pricing"} {
		if section := mappingValue(top, sectionName); section != nil && section.Kind == yaml.MappingNode {
			foundOld, foundNew := false, false
			for i := 0; i < len(section.Content); i += 2 {
				foundOld = foundOld || section.Content[i].Value == op.old
				foundNew = foundNew || section.Content[i].Value == op.new
				add(section.Content[i])
			}
			if foundOld && foundNew {
				return nil, hunk{}, fmt.Errorf("config: %s already contains %s", sectionName, op.new)
			}
		}
	}
	if router := mappingValue(top, "router"); router != nil {
		if classifier := mappingValue(router, "classifier"); classifier != nil {
			add(mappingValue(classifier, "model"))
		}
	}
	if len(matches) == 0 {
		return nil, hunk{}, fmt.Errorf("config: model reference %s was not found", op.old)
	}
	// Apply from bottom to top so line columns remain valid when several references share a line.
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Line > matches[j].Line || matches[i].Line == matches[j].Line && matches[i].Column > matches[j].Column
	})
	var before, after []string
	for _, node := range matches {
		lineIndex := node.Line - 1
		if lineIndex < 0 || lineIndex >= len(lines) {
			return nil, hunk{}, errors.New("config: invalid model reference location")
		}
		line := lineText(lines[lineIndex])
		start := node.Column - 1
		end := start + len(node.Value)
		if node.Style&yaml.SingleQuotedStyle != 0 {
			end += 2
		}
		if node.Style&yaml.DoubleQuotedStyle != 0 {
			end += 2
		}
		if start < 0 || end > len(line) {
			return nil, hunk{}, op.layout(node.Line, "model reference quoting cannot be safely edited")
		}
		replacement := op.new
		if node.Style&yaml.SingleQuotedStyle != 0 {
			replacement = "'" + strings.ReplaceAll(op.new, "'", "''") + "'"
		}
		if node.Style&yaml.DoubleQuotedStyle != 0 {
			replacement = `"` + op.new + `"`
		}
		ending := strings.TrimPrefix(lines[lineIndex], line)
		before = append(before, line)
		line = line[:start] + replacement + line[end:]
		lines[lineIndex] = line + ending
		after = append(after, line)
	}
	return lines, hunk{path: "model references", context: before, added: after}, nil
}

func (op replaceModelReferenceOp) layout(line int, reason string) *LayoutError {
	return &LayoutError{Path: "model references", Line: line, Reason: reason, Snippet: "replace the model reference manually and run `spillway check`"}
}

func (op replaceModelReferenceOp) intend(tree map[string]any) error {
	if tiers, ok := tree["tiers"].(map[string]any); ok {
		for _, value := range tiers {
			if tier, ok := value.(map[string]any); ok {
				if models, ok := tier["models"].([]any); ok {
					for i, model := range models {
						if model == op.old {
							models[i] = op.new
						}
					}
				}
			}
		}
	}
	for _, sectionName := range []string{"models", "pricing"} {
		if section, ok := tree[sectionName].(map[string]any); ok {
			_, hasOld := section[op.old]
			_, hasNew := section[op.new]
			if hasOld && hasNew {
				return fmt.Errorf("config: %s already contains %s", sectionName, op.new)
			}
			if value, exists := section[op.old]; exists {
				delete(section, op.old)
				section[op.new] = value
			}
		}
	}
	if router, ok := tree["router"].(map[string]any); ok {
		if classifier, ok := router["classifier"].(map[string]any); ok && classifier["model"] == op.old {
			classifier["model"] = op.new
		}
	}
	return nil
}

var sequenceItemPrefix = regexp.MustCompile(`^\s*-\s+`)

func (op appendTierModelOp) apply(root *yaml.Node, lines []string, newline string) ([]string, hunk, error) {
	path := "tiers." + op.tier + ".models"
	tiersIndex := mappingIndex(root, "tiers")
	if tiersIndex < 0 {
		return nil, hunk{}, fmt.Errorf("config: tiers.%s does not exist", op.tier)
	}
	tiers := root.Content[tiersIndex+1]
	if !isBlockMapping(tiers) {
		return nil, hunk{}, op.layout(tiers.Line, "tiers is written in flow style or is empty")
	}
	tierIndex := mappingIndex(tiers, op.tier)
	if tierIndex < 0 {
		return nil, hunk{}, fmt.Errorf("config: tiers.%s does not exist", op.tier)
	}
	tier := tiers.Content[tierIndex+1]
	if !isBlockMapping(tier) {
		return nil, hunk{}, op.layout(tier.Line, "tiers."+op.tier+" is written in flow style or is empty")
	}
	modelsIndex := mappingIndex(tier, "models")
	if modelsIndex < 0 {
		return nil, hunk{}, fmt.Errorf("config: %s does not exist", path)
	}
	models := tier.Content[modelsIndex+1]
	if models.Kind != yaml.SequenceNode || models.Style&yaml.FlowStyle != 0 || len(models.Content) == 0 {
		return nil, hunk{}, op.layout(models.Line, path+" is written in flow style or is empty")
	}
	for _, item := range models.Content {
		if item.Value == op.model {
			return nil, hunk{}, fmt.Errorf("config: %s already lists %s", path, op.model)
		}
	}
	last := models.Content[len(models.Content)-1]
	prefix := sequenceItemPrefix.FindString(lineText(lines[last.Line-1]))
	if last.Kind != yaml.ScalarNode || prefix == "" {
		return nil, hunk{}, op.layout(last.Line, "the last entry of "+path+" is not a one-line list item")
	}
	next := followingLine(len(lines)+1, []keyAt{{tier, modelsIndex}, {tiers, tierIndex}, {root, tiersIndex}})
	return splice(lines, lastContentLine(lines, last.Line-1, next), []string{prefix + op.model}, newline, path)
}

func (op appendTierModelOp) layout(line int, reason string) *LayoutError {
	snippet := "tiers:\n  " + op.tier + ":\n    models:\n      - " + op.model + "\n"
	return &LayoutError{Path: "tiers." + op.tier + ".models", Line: line, Reason: reason, Snippet: snippet}
}

func (op appendTierModelOp) intend(tree map[string]any) error {
	tiers, _ := tree["tiers"].(map[string]any)
	tier, _ := tiers[op.tier].(map[string]any)
	models, ok := tier["models"].([]any)
	if !ok {
		return fmt.Errorf("config: internal error: tiers.%s.models is not a list", op.tier)
	}
	tier["models"] = append(models, op.model)
	return nil
}

// keyAt is a key by its index in a mapping node.
type keyAt struct {
	mapping *yaml.Node
	index   int
}

// followingLine returns the line of the first key that follows any key in
// chain, innermost first, or eof when none follows.
func followingLine(eof int, chain []keyAt) int {
	for _, key := range chain {
		if key.index+2 < len(key.mapping.Content) {
			return key.mapping.Content[key.index+2].Line
		}
	}
	return eof
}

func isBlockMapping(n *yaml.Node) bool {
	return n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0
}

// usesAnchors reports anchors, aliases or merge keys anywhere in the tree; a
// line splice next to them could change values elsewhere in the file.
func usesAnchors(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode || n.Tag == "!!merge" {
		return true
	}
	for _, child := range n.Content {
		if usesAnchors(child) {
			return true
		}
	}
	return false
}

func providerAbsent(root *yaml.Node, name string) error {
	section := mappingValue(root, "providers")
	if section == nil || section.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(section.Content); i += 2 {
		if section.Content[i].Value == name {
			return &ProviderExistsError{Name: name, Line: section.Content[i].Line}
		}
	}
	return nil
}

func mappingIndex(mapping *yaml.Node, name string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return i
		}
	}
	return -1
}

// childIndent returns the indentation step the file already uses inside entries.
func childIndent(section *yaml.Node) string {
	for i := 0; i+1 < len(section.Content); i += 2 {
		key, value := section.Content[i], section.Content[i+1]
		if value.Kind == yaml.MappingNode && value.Style&yaml.FlowStyle == 0 && len(value.Content) > 0 {
			return strings.Repeat(" ", value.Content[0].Column-key.Column)
		}
	}
	return "  "
}

// lastContentLine returns the last line in (start, next) that is not blank or a
// comment; trailing comments belong to the following key.
func lastContentLine(lines []string, start, next int) int {
	for line := next - 1; line > start; line-- {
		if !isBlankOrComment(lines[line-1]) {
			return line
		}
	}
	return start
}

// splice inserts added after the 1-based line after and records the preview hunk.
func splice(lines []string, after int, added []string, newline, path string) ([]string, hunk, error) {
	out := make([]string, 0, len(lines)+len(added)+1)
	out = append(out, lines[:after]...)
	if after > 0 && !strings.HasSuffix(out[after-1], "\n") {
		out[after-1] += newline
	}
	for _, line := range added {
		out = append(out, line+newline)
	}
	out = append(out, lines[after:]...)
	h := hunk{path: path, added: added}
	for line := max(after-3, 0); line < after; line++ {
		h.context = append(h.context, lineText(lines[line]))
	}
	return out, h, nil
}

// localKeyLiterals returns local key values written in the file; ${VAR}
// references are not secrets and stay visible in previews.
func localKeyLiterals(root *yaml.Node) []redact.Secret {
	var secrets []redact.Secret
	for _, secret := range secretsFromNode(root) {
		if !providerKeyReference.MatchString(secret.Reveal()) {
			secrets = append(secrets, secret)
		}
	}
	return secrets
}
