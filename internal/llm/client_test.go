package llm

import "testing"

func TestResolveBaseURLUsesDefaultForBlankValue(t *testing.T) {
	for _, value := range []string{"", " ", "\t"} {
		if got := resolveBaseURL(value); got != defaultBaseURL {
			t.Fatalf("resolveBaseURL(%q) = %q, want %q", value, got, defaultBaseURL)
		}
	}
}

func TestResolveBaseURLPreservesConfiguredEndpoint(t *testing.T) {
	const endpoint = "https://proxy.example/v1/"
	if got := resolveBaseURL("  " + endpoint + " "); got != endpoint {
		t.Fatalf("resolveBaseURL(custom) = %q, want %q", got, endpoint)
	}
}
