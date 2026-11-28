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

// rotatingSignatureParams are query keys that rotate in presigned URLs. They
// must not be part of a task identity, or a refreshed link looks like a new
// download and the agent loses its resumable progress.
var rotatingSignatureParams = map[string]bool{
	"sp": true, "sv": true, "sr": true, "sig": true, "se": true,
	"st": true, "skt": true, "ske": true, "skoid": true, "sktid": true,
	"sks": true, "skv": true, "jwt": true,
	"X-Amz-Signature": true, "X-Amz-Date": true, "X-Amz-Expires": true,
	"X-Amz-Credential": true, "X-Amz-Algorithm": true, "X-Amz-SignedHeaders": true,
	"X-Goog-Signature": true, "X-Goog-Date": true, "X-Goog-Expires": true,
	"X-Goog-Credential": true, "X-Goog-Algorithm": true, "X-Goog-SignedHeaders": true,
	"Signature": true, "Expires": true, "Key-Pair-Id": true, "Policy": true,
	"token": true,
	// These only override response headers (the local filename/type), never
	// the bytes, so a CDN rotating them must not change task identity.
	"response-content-disposition": true, "response-content-type": true,
}

// signatureHost reports whether raw's query is expected to contain rotating
// signature credentials. GitHub was the original case; HuggingFace's xet/LFS
// bridge (cas-bridge.xethub.hf.co) and S3/GCS presigned URLs have the same
// property, and a 3GB model download showed the cost of ignoring them: a
// refreshed link created a new task and discarded 1.9GB of progress.
func signatureHost(u *url.URL) bool {
	if u == nil {
		return false
	}
	if GitHubHost(u) {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return h == "huggingface.co" || strings.HasSuffix(h, ".huggingface.co") ||
		h == "hf.co" || strings.HasSuffix(h, ".hf.co") ||
		strings.HasSuffix(h, ".xethub.hf.co")
}

// CanonicalURL strips rotating signature parameters so the same asset keeps
// one identity across link refreshes. It deliberately keeps non-signature
// params such as ?id= or ?ref= — those select the actual bytes. URLs from
// hosts without rotating signatures are returned unchanged.
func CanonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !signatureHost(u) {
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		if rotatingSignatureParams[k] || rotatingSignatureParams[strings.ToLower(k)] {
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

// GitHubThrottled reports whether this URL is on a GitHub CDN host whose
// small-file concurrency should stay conservative. Measured release-asset
// paths were jittery with 16 connections and faster at 4 for files under
// 256MB; api.github.com is an API endpoint and is not treated this way.
func GitHubThrottled(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !GitHubHost(u) {
		return false
	}
	h := strings.ToLower(u.Hostname())
	// Raw files and codeload archives also come from GitHub's CDN; prefer the
	// conservative cap there too. api.github.com can handle ranges cheaply.
	return h != "api.github.com"
}
