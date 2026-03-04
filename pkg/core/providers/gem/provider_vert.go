package gem

// NewVertex constructs the Vertex-backed Gemini provider (`gem-vert`).
func NewVertex(apiKey string, opts ...Option) *Provider {
	return newProvider(providerVert, backendModeVertex, apiKey, opts...)
}
