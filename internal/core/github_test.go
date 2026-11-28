package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubURLRecognition(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/yoyo636/opendrain/releases/download/v0.1.0/f.bin": true,
		"https://api.github.com/repos/a/b/releases/assets/1":                  true,
		"https://raw.githubusercontent.com/a/b/main/README.md":                true,
		"https://release-assets.githubusercontent.com/x?sig=abc":              true,
		"https://codeload.github.com/a/b/tar.gz/refs/heads/main":              true,
		"https://example.org/file.bin":                                        false,
		"https://notgithub.com.evil.org/x":                                    false,
	}
	for raw, want := range cases {
		if got := IsGitHubURL(raw); got != want {
			t.Errorf("IsGitHubURL(%q)=%v want %v", raw, got, want)
		}
	}
}

func TestCanonicalURLStripsSignaturesButKeepsSelectorParams(t *testing.T) {
	signed := "https://release-assets.githubusercontent.com/asset?sp=r&sv=2018-11-09&sig=abc&se=2026-11-23T14%3A39%3A57Z&id=42&token=xyz"
	want := "https://release-assets.githubusercontent.com/asset?id=42"
	if got := CanonicalURL(signed); got != want {
		t.Errorf("CanonicalURL = %q, want %q", got, want)
	}
	plain := "https://example.org/f.bin?sig=abc&id=7"
	if got := CanonicalURL(plain); got != plain {
		t.Errorf("non-GitHub URL must be unchanged: %q", got)
	}
	unrelated := "https://github.com/a/b/releases/download/v1/f.bin?ref=main"
	if got := CanonicalURL(unrelated); got != unrelated {
		t.Errorf("non-signature params must survive: %q", got)
	}
}

func TestIDStableAcrossSignatureRefresh(t *testing.T) {
	target := "/tmp/model.safetensors"
	a := IDFor("https://release-assets.githubusercontent.com/asset?id=42&sig=old&se=2026-11-23", target)
	b := IDFor("https://release-assets.githubusercontent.com/asset?sig=new&se=2026-11-24&id=42", target)
	if a != b {
		t.Error("task ID must stay the same when only the signature rotates")
	}
	if a == IDFor("https://release-assets.githubusercontent.com/asset?id=43&sig=old", target) {
		t.Error("different selector param must produce a different ID")
	}
}

func TestGitHubAuthStripsTokenOnRedirectTarget(t *testing.T) {
	var seen []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	tr := &githubAuthTransport{base: http.DefaultTransport, token: "SECRET"}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", target.URL, nil)
	req.Header.Set("Authorization", "Bearer original")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 1 || seen[0] != "" {
		t.Errorf("Authorization must be stripped on non-GitHub host, got %v", seen)
	}
}

func TestWrapGitHubAuthAddsTokenForAPIHost(t *testing.T) {
	// The transport is inspection-only here; no network call needed.
	tr := &githubAuthTransport{base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer TOK" {
			t.Errorf("auth header = %q", got)
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}), token: "TOK"}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://api.github.com/repos/a/b", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestSafeRedirectPolicyCapsAndRefusesDowngrade(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/", nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i], _ = http.NewRequestWithContext(context.Background(), "GET", "https://x/", nil)
	}
	if err := SafeRedirectPolicy(req, via); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("expected redirect cap, got %v", err)
	}
	down, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/", nil)
	prev, _ := http.NewRequestWithContext(context.Background(), "GET", "https://x/", nil)
	if err := SafeRedirectPolicy(down, []*http.Request{prev}); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("expected downgrade refusal, got %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCanonicalURLHandlesHuggingFaceRotatingSignatures(t *testing.T) {
	a := "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/blob" +
		"?Expires=1&Policy=p1&Signature=s1&Key-Pair-Id=k&xip=u&download=true"
	b := "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/blob" +
		"?Expires=2&Policy=p2&Signature=s2&Key-Pair-Id=k&xip=u&download=true"
	if CanonicalURL(a) != CanonicalURL(b) {
		t.Errorf("HF/xet signature refresh must not change identity: %q vs %q", CanonicalURL(a), CanonicalURL(b))
	}
	got := CanonicalURL(a)
	if strings.Contains(got, "Signature=") || strings.Contains(got, "Expires=") {
		t.Errorf("signature params still present: %q", got)
	}
	if !strings.Contains(got, "download=true") {
		t.Errorf("non-signature params must survive: %q", got)
	}
	other := "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/other?Expires=1&Signature=s1"
	if CanonicalURL(a) == CanonicalURL(other) {
		t.Error("different blob paths must stay different tasks")
	}
	plain := "https://example.org/f?sig=1&id=7"
	if CanonicalURL(plain) != plain {
		t.Errorf("unknown host must be unchanged: %q", CanonicalURL(plain))
	}
}

func TestGitHubAuthResolvesTokenLazily(t *testing.T) {
	calls := 0
	tr := &githubAuthTransport{
		base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		}),
		resolve: func() string { calls++; return "TOK" },
	}
	do := func(raw string) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", raw, nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	do("https://example.org/file.bin")
	if calls != 0 {
		t.Errorf("non-GitHub request must not resolve a token, calls=%d", calls)
	}
	do("https://api.github.com/repos/a/b")
	if calls != 1 {
		t.Errorf("first GitHub request should resolve once, calls=%d", calls)
	}
	do("https://api.github.com/repos/a/b")
	if calls != 1 {
		t.Errorf("token must be cached, calls=%d", calls)
	}
}
