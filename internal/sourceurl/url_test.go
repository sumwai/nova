package sourceurl

import "testing"

func TestNormalizeMergesEquivalentWritings(t *testing.T) {
	want := "https://example.com/profiles"
	equivalents := []string{
		"https://example.com/profiles",
		"https://EXAMPLE.com/profiles",
		"https://example.com:443/profiles",
		"https://example.com./profiles",
		"https://EXAMPLE.COM.:443/profiles",
		"https://example.com/profiles/",
		"https://example.com/profiles#frag",
		"HTTPS://example.com/profiles",
	}
	for _, raw := range equivalents {
		if got := Normalize(raw); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

func TestNormalizeKeepsDistinctSourcesDistinct(t *testing.T) {
	distinct := []string{
		"https://example.com:8443/profiles",
		"http://example.com/profiles",
		"https://other.example/profiles",
		"https://example.com/profiles?tenant=a",
		"https://example.com/other",
	}
	seen := map[string]string{}
	for _, raw := range distinct {
		got := Normalize(raw)
		if first, ok := seen[got]; ok {
			t.Errorf("%q 与 %q 归一到同一结果 %q，但它们不是同一个源", first, raw, got)
		}
		seen[got] = raw
	}
}

func TestCanonicalHost(t *testing.T) {
	tests := []struct {
		scheme   string
		hostport string
		want     string
	}{
		{"https", "example.com", "example.com"},
		{"https", "Example.COM.", "example.com"},
		{"https", "example.com:443", "example.com"},
		{"https", "example.com:8443", "example.com:8443"},
		{"https", "[::1]:443", "[::1]"},
		{"https", "[::1]:8443", "[::1]:8443"},
	}
	for _, tt := range tests {
		if got := CanonicalHost(tt.scheme, tt.hostport); got != tt.want {
			t.Errorf("CanonicalHost(%q, %q) = %q，期望 %q", tt.scheme, tt.hostport, got, tt.want)
		}
	}
}
