package providers

// Preset describes a built-in provider's defaults and accepted config.
type Preset struct {
	Name               string
	DefaultBaseURL     string
	KeyEnv             string
	RequiresKey        bool
	AcceptsKey         bool
	FreeTierAttestable bool
	SuggestedModel     string
	// SuggestedCapabilities is the ADR 0007 declaration init writes for SuggestedModel.
	SuggestedCapabilities []string
}

// Presets returns the built-in provider presets in stable name order.
func Presets() []Preset {
	presets := builtInPresets()
	return append([]Preset(nil), presets[:]...)
}

// LookupPreset returns the preset for a built-in provider.
func LookupPreset(name string) (Preset, bool) {
	for _, preset := range builtInPresets() {
		if preset.Name == name {
			return preset, true
		}
	}
	return Preset{}, false
}

func builtInPresets() [4]Preset {
	return [...]Preset{
		{Name: "gemini", DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY", RequiresKey: true, AcceptsKey: true, FreeTierAttestable: true, SuggestedModel: "gemini-3.5-flash", SuggestedCapabilities: []string{"tools", "vision", "json_mode", "structured_outputs"}},
		{Name: "groq", DefaultBaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY", RequiresKey: true, AcceptsKey: true, FreeTierAttestable: true, SuggestedModel: "openai/gpt-oss-120b", SuggestedCapabilities: []string{"tools"}},
		{Name: "ollama", DefaultBaseURL: "http://localhost:11434", SuggestedModel: "llama3.2:3b", SuggestedCapabilities: []string{"tools"}},
		{Name: "openrouter", DefaultBaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", RequiresKey: true, AcceptsKey: true, SuggestedModel: "google/gemma-4-31b-it:free", SuggestedCapabilities: []string{"tools"}},
	}
}
