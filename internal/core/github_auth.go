package core

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	tokenOnce sync.Once
	ghToken   string
)

// GitHubToken resolves a token without ever putting it in argv: prefer
// GITHUB_TOKEN / GH_TOKEN, then `gh auth token`. An empty result is fine —
// public assets work without a token.
func GitHubToken() string {
	tokenOnce.Do(func() {
		ghToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		if ghToken == "" {
			ghToken = strings.TrimSpace(os.Getenv("GH_TOKEN"))
		}
		if ghToken == "" {
			if p, err := exec.LookPath("gh"); err == nil {
				if out, err := exec.CommandContext(context.Background(), p, "auth", "token").Output(); err == nil {
					ghToken = strings.TrimSpace(string(out))
				}
			}
		}
	})
	return ghToken
}

// githubAuthTransport attaches Authorization only to GitHub-owned API hosts.
// GitHub releases redirect to release-assets.githubusercontent.com with a
// presigned URL; sending the token there would leak it and is rejected anyway.
type githubAuthTransport struct {
	base   http.RoundTripper
	token  string
	secure bool // strip credentials even on non-GitHub redirects
}

func (t *githubAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	h := strings.ToLower(r.URL.Hostname())
	githubAPI := h == "github.com" || h == "api.github.com"
	if !githubAPI || t.secure {
		r.Header.Del("Authorization")
	} else if t.token != "" && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

// WrapGitHubAuth returns a transport that adds a GitHub token for API hosts
// and strips it everywhere else. Pass-through when no token is available.
func WrapGitHubAuth(base http.RoundTripper) http.RoundTripper {
	tok := GitHubToken()
	if tok == "" || base == nil {
		return base
	}
	return &githubAuthTransport{base: base, token: tok}
}

// SafeRedirectPolicy caps redirects and refuses https -> http downgrades.
// GitHub's own redirects are https, so this does not break release assets.
func SafeRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return &url.Error{Op: "redirect", URL: req.URL.String(), Err: errTooManyRedirects}
	}
	if req.URL.Scheme == "http" && len(via) > 0 && via[len(via)-1].URL.Scheme == "https" {
		return &url.Error{Op: "redirect", URL: req.URL.String(), Err: errInsecureRedirect}
	}
	return nil
}

type redirectError string

func (e redirectError) Error() string { return string(e) }

const (
	errTooManyRedirects = redirectError("too many redirects")
	errInsecureRedirect = redirectError("refused https to http downgrade")
)
