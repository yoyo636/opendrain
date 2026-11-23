package core

import (
	"net/url"
	"strings"
)

// GitHubHost reports whether u is a host owned by GitHub. These hosts share
// two traits that matter for an agent downloader: they often sit behind a
// slow/throttled CDN path, and release-assets URLs are short-lived signed
// redirects of a stable logical URL.
func GitHubHost(u *url.URL) bool {
	if u == nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "github.com" || h == "api.github.com" || h == "raw.githubusercontent.com" ||
		h == "gist.githubusercontent.com" || h == "objects.githubusercontent.com" ||
		h == "release-assets.githubusercontent.com" || h == "codeload.github.com" ||
		h == "githubusercontent.com" {
		return true
	}
	return strings.HasSuffix(h, ".githubusercontent.com") || strings.HasSuffix(h, ".github.com")
}

// IsGitHubURL parses raw and reports whether it points at GitHub.
func IsGitHubURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && GitHubHost(u)
}

// githubSignatureParams are query keys that rotate in presigned URLs. They
// must not be part of a task identity, or a refreshed link looks like a new
// download and the agent loses its resumable progress.
var githubSignatureParams = map[string]bool{
	"sp": true, "sv": true, "sr": true, "sig": true, "se": true,
	"st": true, "skt": true, "ske": true, "skoid": true, "sktid": true,
	"sks": true, "skv": true, "jwt": true,
	"X-Amz-Signature": true, "X-Amz-Date": true, "X-Amz-Expires": true,
	"X-Amz-Credential": true, "X-Amz-Algorithm": true, "X-Amz-SignedHeaders": true,
	"X-Goog-Signature": true, "X-Goog-Date": true, "X-Goog-Expires": true,
	"X-Goog-Credential": true, "X-Goog-Algorithm": true, "X-Goog-SignedHeaders": true,
	"Signature": true, "Expires": true, "Key-Pair-Id": true, "Policy": true,
	"token": true,
}

// CanonicalURL strips rotating signature parameters from a GitHub URL so the
// same asset keeps one identity across link refreshes. It deliberately keeps
// non-signature params such as ?id= or ?ref= — those select the actual bytes.
// Non-GitHub URLs are returned unchanged.
func CanonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !GitHubHost(u) {
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		if githubSignatureParams[k] || githubSignatureParams[strings.ToLower(k)] {
			q.Del(k)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// UseSingleStream reports whether this GitHub URL should avoid ranged
// concurrency. A measured CDN path to release-assets showed wildly unstable
// per-connection behavior (4s..200s for the same file), so 16-way splitting
// amplified jitter instead of improving throughput.
func UseSingleStream(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !GitHubHost(u) {
		return false
	}
	h := strings.ToLower(u.Hostname())
	// Raw files and codeload archives also come from GitHub's CDN; prefer a
	// single stream there too. api.github.com can handle ranges cheaply.
	return h != "api.github.com"
}
