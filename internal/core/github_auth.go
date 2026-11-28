package core

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	tokenOnce sync.Once
	ghToken   string
)

// GitHubToken resolves a token without ever putting it in argv: prefer
// GITHUB_TOKEN / GH_TOKEN, then `gh auth token`. An empty result is fine —
// public assets work without a token. The gh subprocess is bounded so a
// stuck credential prompt cannot wedge a download that never needed GitHub.
func GitHubToken() string {
	tokenOnce.Do(func() {
		ghToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		if ghToken == "" {
			ghToken = strings.TrimSpace(os.Getenv("GH_TOKEN"))
		}
		if ghToken == "" {
			if p, err := exec.LookPath("gh"); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if out, err := exec.CommandContext(ctx, p, "auth", "token").Output(); err == nil {
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
//
// Resolution is lazy: a plain non-GitHub download never spawns `gh auth
// token`, which used to add startup latency (and a hang risk) to every fetch
// on a machine that happens to have gh installed.
type githubAuthTransport struct {
	base    http.RoundTripper
	token   string
	resolve func() string
	once    sync.Once
}

func (t *githubAuthTransport) bearer() string {
	if t.token != "" {
		return t.token
	}
	if t.resolve != nil {
		t.once.Do(func() { t.token = strings.TrimSpace(t.resolve()) })
	}
	return t.token
}

func (t *githubAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	h := strings.ToLower(r.URL.Hostname())
	githubAPI := h == "github.com" || h == "api.github.com"
	if !githubAPI {
		// Strip caller-supplied credentials too: a cross-host redirect must
		// never carry a token to the CDN.
		r.Header.Del("Authorization")
	} else if r.Header.Get("Authorization") == "" {
		if tok := t.bearer(); tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return t.base.RoundTrip(r)
}

// WrapGitHubAuth returns a transport that adds a GitHub token for API hosts
// and strips it everywhere else. It always wraps (even without a token) so a
// caller-provided Authorization header is still removed on non-GitHub hosts.
func WrapGitHubAuth(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		return nil
	}
	return &githubAuthTransport{base: base, resolve: GitHubToken}
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
