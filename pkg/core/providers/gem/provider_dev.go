package gem

// NewDeveloper constructs the Gemini Developer API-backed provider (`gem-dev`).
func NewDeveloper(apiKey string, opts ...Option) *Provider {
	return newProvider(providerDev, backendModeDeveloper, apiKey, opts...)
}
