package excel

// DefaultHeaders returns the client-identity headers the Basispoints backend
// expects from the official Excel add-in.
//
// The backend gates this endpoint on add-in identity, not only on the OAuth
// token, so these must be present even though the request is relayed by Codex.
// A credential entry may override any of them through its configured headers.
func DefaultHeaders() map[string]string {
	return map[string]string{
		"x-basispoints-auth-mode":                             "chatgpt",
		"x-openai-internal-basispoints-client-agent-profile":  "excel",
		"x-openai-internal-basispoints-client-editor":         "excel",
		"x-openai-internal-basispoints-client-host":           "office",
		"x-openai-internal-basispoints-client-platform":       "excel",
		"x-openai-internal-basispoints-client-platform-class": "PC",
		"x-openai-internal-basispoints-client-product":        "basispoints-excel-plugin",
		"x-openai-internal-basispoints-client-runtime":        "desktop",
		"x-openai-internal-basispoints-office-host":           "Excel",
		"x-openai-internal-basispoints-office-platform":       "PC",
		"x-stainless-arch":                                    "unknown",
		"x-stainless-lang":                                    "js",
		"x-stainless-os":                                      "Unknown",
		"x-stainless-package-version":                         "6.31.0",
		"x-stainless-retry-count":                             "0",
		"x-stainless-runtime":                                 "browser:chrome",
		"Origin":                                              "https://bps.openai.com",
	}
}

// DefaultBaseURL is the add-in backend endpoint.
const DefaultBaseURL = "https://bps.openai.com"

// ResponsesPath is appended to the base URL for model invocation.
const ResponsesPath = "/basispoints/api/responses"
