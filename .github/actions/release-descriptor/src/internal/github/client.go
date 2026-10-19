// Package github verifies branch ancestry through the GitHub REST API (D-025, D-041).
package github

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/metrics"
)

// DefaultAPI is the public GitHub REST endpoint.
const DefaultAPI = "https://api.github.com"

// DefaultWeb is the public GitHub web endpoint.
const DefaultWeb = "https://github.com"

// Result is persisted on a candidate; response bodies and credentials are never retained.
type Result struct {
	State      string    `json:"state"`
	CheckedAt  time.Time `json:"checkedAt"`
	BranchHead string    `json:"branchHead,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	ETag       string    `json:"etag,omitempty"`
}

// Head is the project's conditional branch-head cache.
type Head struct {
	SHA       string    `json:"sha"`
	ETag      string    `json:"etag,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Config supplies the endpoint, optional credential file, HTTP transport and clock.
type Config struct {
	APIURL, WebURL, TokenFile string
	HTTPClient                *http.Client
	Metrics                   *metrics.Metrics
	Now                       func() time.Time
}

type repoExistence struct {
	found bool
	until time.Time
}

// Client shares caches and a rate-limit gate without holding locks during I/O (06 §6).
type Client struct {
	metrics                    *metrics.Metrics
	repos                      map[string]repoExistence
	mu                         sync.Mutex
	api, web, tokenFile, token string
	http                       *http.Client
	now                        func() time.Time
	reset                      time.Time
	lru                        *list.List
	cache                      map[string]*list.Element
}

// New reads the optional token at startup. Redirects are refused to keep credentials on the API host (F20).
func New(cfg Config) (*Client, error) {
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPI
	}
	if cfg.WebURL == "" {
		cfg.WebURL = DefaultWeb
	}
	for _, raw := range []string{cfg.APIURL, cfg.WebURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("github: invalid endpoint")
		}
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second}
	}
	copyHTTP := *h
	copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copyHTTP.Timeout == 0 || copyHTTP.Timeout > 15*time.Second {
		copyHTTP.Timeout = 15 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &Client{metrics: cfg.Metrics, repos: map[string]repoExistence{}, api: strings.TrimRight(cfg.APIURL, "/"), web: cfg.WebURL, tokenFile: cfg.TokenFile, http: &copyHTTP, now: cfg.Now, lru: list.New(), cache: map[string]*list.Element{}}
	if err := c.ReloadToken(); err != nil {
		return nil, err
	}
	return c, nil
}

// ReloadToken atomically reloads the optional file on SIGHUP, retaining the previous token on failure.
func (c *Client) ReloadToken() error {
	if c.tokenFile == "" {
		return nil
	}
	f, e := os.Open(c.tokenFile)
	if e != nil {
		return errors.New("github: cannot read token file")
	}
	defer func() { _ = f.Close() }()
	info, e := f.Stat()
	// Group read is allowed for Kubernetes Secret mounts (0440 with fsGroup, 19 §19.3); group write and any world
	// access are not.
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return errors.New("github: token file must not be group-writable or world-accessible")
	}
	b, e := io.ReadAll(io.LimitReader(f, 4097))
	if e != nil {
		return errors.New("github: cannot read token file")
	}
	token := strings.TrimSpace(string(b))
	if token == "" || len(b) > 4096 || strings.ContainsAny(token, "\r\n") {
		return errors.New("github: invalid token file")
	}
	u, _ := url.Parse(c.api)
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("github: token requires HTTPS or a loopback API endpoint")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	return nil
}

// Repository validates source.repo and returns its escaped owner/repo path (§20.2).
func Repository(raw, web string) (string, error) {
	if web == "" {
		web = DefaultWeb
	}
	w, we := url.Parse(web)
	u, e := url.Parse(raw)
	if e != nil || we != nil || u.Scheme != "https" || u.Host != w.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("only GitHub repositories are supported")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", errors.New("only GitHub repositories are supported")
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\%?#") {
			return "", errors.New("only GitHub repositories are supported")
		}
	}
	return url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

// ReleaseURLs derives links from the immutable release repository (R32, §20.4).
// Invalid repositories, and commits that are not full SHAs (D-053), omit both links; web defaults to DefaultWeb.
func ReleaseURLs(raw, commit, web string) (commitURL, manifestsURL string) {
	path, err := Repository(raw, web)
	if err != nil || !FullSHA(commit) {
		return "", ""
	}
	if web == "" {
		web = DefaultWeb
	}
	repo := strings.TrimRight(web, "/") + "/" + path
	return repo + "/commit/" + url.PathEscape(commit), repo + "/tree/" + url.PathEscape(commit) + "/deploy"
}

func (c *Client) request(ctx context.Context, path, etag string, metadataOnly bool) (int, http.Header, []byte, error) {
	c.mu.Lock()
	reset, token := c.reset, c.token
	c.mu.Unlock()
	if c.now().Before(reset) {
		return 0, nil, nil, fmt.Errorf("rate limited until %s", reset.UTC().Format(time.RFC3339))
	}
	// §18.2: one outcome per actual HTTP attempt; cache hits and the local rate gate do not count.
	outcome := "error"
	defer func() {
		if c.metrics != nil {
			c.metrics.GitHubRequests.WithLabelValues(outcome).Inc()
		}
	}()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.api+path, nil)
	if e != nil {
		return 0, nil, nil, errors.New("invalid GitHub request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if etag = validETag(etag); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, e := c.http.Do(req)
	if e != nil {
		return 0, nil, nil, errors.New("GitHub request failed")
	}
	defer func() { _ = res.Body.Close() }()
	if (res.StatusCode == 403 || res.StatusCode == 429) && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "") {
		outcome = "rateLimited"
		var reset time.Time
		if sec, e := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
			reset = time.Unix(sec, 0)
		}
		if retry := res.Header.Get("Retry-After"); retry != "" {
			if sec, e := strconv.Atoi(retry); e == nil {
				reset = maxTime(reset, c.now().Add(time.Duration(sec)*time.Second))
			} else if t, e := http.ParseTime(retry); e == nil {
				reset = maxTime(reset, t)
			}
		}
		if !reset.After(c.now()) {
			reset = c.now().Add(time.Minute)
		}
		c.mu.Lock()
		c.reset = maxTime(c.reset, reset)
		reset = c.reset
		c.mu.Unlock()
		return 0, nil, nil, fmt.Errorf("rate limited until %s", reset.UTC().Format(time.RFC3339))
	}
	var b []byte
	if res.StatusCode == 200 && metadataOnly {
		b, e = compareMetadata(io.LimitReader(res.Body, 1<<20))
	} else {
		b, e = io.ReadAll(io.LimitReader(res.Body, 1<<20))
	}
	if e != nil {
		return 0, nil, nil, errors.New("cannot read GitHub response")
	}
	switch res.StatusCode {
	case http.StatusOK:
		outcome = "ok"
	case http.StatusNotModified:
		outcome = "notModified"
	}
	return res.StatusCode, res.Header, b, nil
}
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Compare proves that commit is identical to or behind the unambiguous branch-head SHA. Only positive results enter the bounded LRU (§20.3).
func (c *Client) Compare(ctx context.Context, repo, head, commit string, prev Result) Result {
	out := Result{State: "error", CheckedAt: c.now().UTC()}
	if !FullSHA(head) || !FullSHA(commit) {
		out.Detail = "lineage requires full commit SHAs"
		return out
	}
	out.BranchHead = head
	path, e := Repository(repo, c.web)
	if e != nil {
		out.Detail = e.Error()
		return out
	}
	key := repo + "\x00" + head + "\x00" + commit
	c.mu.Lock()
	if el := c.cache[key]; el != nil {
		c.lru.MoveToFront(el)
		out.State = "onBranch"
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()
	etag := ""
	if prev.BranchHead == head && (prev.State == "onBranch" || prev.State == "notOnBranch") {
		etag = prev.ETag
	}
	code, h, b, e := c.request(ctx, "/repos/"+path+"/compare/"+url.PathEscape(head)+"..."+url.PathEscape(commit)+"?per_page=1", etag, true)
	if e != nil {
		out.Detail = e.Error()
		return out
	}
	out.ETag = validETag(h.Get("ETag"))
	switch code {
	case 200:
		var v struct {
			Status string `json:"status"`
			Base   struct {
				SHA string `json:"sha"`
			} `json:"base_commit"`
		}
		if json.Unmarshal(b, &v) != nil || !FullSHA(v.Base.SHA) || v.Base.SHA != head {
			out.Detail = "invalid compare response"
			return out
		}
		out.BranchHead = v.Base.SHA
		switch v.Status {
		case "identical", "behind":
			out.State = "onBranch"
		case "ahead", "diverged":
			out.State = "notOnBranch"
		default:
			out.Detail = "invalid compare status"
		}
	case 304:
		if prev.BranchHead == head && (prev.State == "onBranch" || prev.State == "notOnBranch") {
			out = prev
			out.ETag = validETag(out.ETag)
		} else {
			out.Detail = "compare returned 304 without cached result"
		}
	case 404:
		exists, err := c.repositoryExists(ctx, path)
		if err != nil {
			out.Detail = err.Error()
			return out
		}
		if !exists {
			out.Detail = c.missingRepositoryDetail()
		} else {
			out.State = "notOnBranch"
			out.Detail = "unknown commit"
		}
	case 422:
		out.State = "notOnBranch"
		out.Detail = "unknown commit or no common ancestor"
	default:
		out.Detail = fmt.Sprintf("GitHub HTTP %d", code)
	}
	if out.State == "onBranch" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if el := c.cache[key]; el != nil {
			c.lru.MoveToFront(el)
			return out
		}
		c.cache[key] = c.lru.PushFront(key)
		if c.lru.Len() > 1000 {
			el := c.lru.Back()
			delete(c.cache, el.Value.(string))
			c.lru.Remove(el)
		}
	}
	return out
}

// BranchHead conditionally reads a branch while negative candidates exist (§20.2).
func (c *Client) BranchHead(ctx context.Context, repo, branch string, prev Head) (Head, error) {
	path, e := Repository(repo, c.web)
	if e != nil {
		return prev, e
	}
	code, h, b, e := c.request(ctx, "/repos/"+path+"/branches/"+url.PathEscape(branch), prev.ETag, false)
	if e != nil {
		return prev, e
	}
	if code == 304 && FullSHA(prev.SHA) {
		prev.CheckedAt = c.now().UTC()
		prev.ETag = validETag(prev.ETag)
		return prev, nil
	}
	if code == 404 {
		exists, err := c.repositoryExists(ctx, path)
		if err != nil {
			return prev, err
		}
		if !exists {
			return prev, errors.New(c.missingRepositoryDetail())
		}
	}
	if code != 200 {
		return prev, fmt.Errorf("GitHub HTTP %d", code)
	}
	var v struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if json.Unmarshal(b, &v) != nil || !FullSHA(v.Commit.SHA) {
		return prev, errors.New("invalid branch response")
	}
	return Head{SHA: v.Commit.SHA, ETag: validETag(h.Get("ETag")), CheckedAt: c.now().UTC()}, nil
}

// The head-first path and compare disambiguation share the same bounded-time probe cache.
func (c *Client) repositoryExists(ctx context.Context, path string) (bool, error) {
	c.mu.Lock()
	exists, ok := c.repos[path]
	c.mu.Unlock()
	if ok && c.now().Before(exists.until) {
		return exists.found, nil
	}
	status, _, _, err := c.request(ctx, "/repos/"+path, "", false)
	if err != nil {
		return false, err
	}
	if status != 200 && status != 404 {
		return false, fmt.Errorf("GitHub HTTP %d", status)
	}
	exists = repoExistence{found: status == 200, until: c.now().Add(30 * time.Second)}
	c.mu.Lock()
	c.repos[path] = exists
	c.mu.Unlock()
	return exists.found, nil
}
func (c *Client) missingRepositoryDetail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return "repository not found or private; set --github-token-file"
	}
	return "repository not found or inaccessible to configured token"
}

// FullSHA accepts only bounded lowercase Git object IDs (D-044).
func FullSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// ETags are opaque HTTP entity tags, at most 256 ASCII bytes. Invalid tags disable conditional reads.
func validETag(s string) string {
	if len(s) > 256 {
		return ""
	}
	raw := strings.TrimPrefix(s, "W/")
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return ""
	}
	for _, c := range raw[1 : len(raw)-1] {
		if c < 0x21 || c > 0x7e || c == '"' {
			return ""
		}
	}
	return s
}

// compareMetadata stops once the status and base are known. GitHub puts these
// before commits/files; a large divergent diff cannot poison the retry budget.
func compareMetadata(r io.Reader) ([]byte, error) {
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid compare response")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
		if key == "status" || key == "base_commit" {
			fields[key.(string)] = value
		}
		if len(fields) == 2 {
			return json.Marshal(fields)
		}
	}
	return json.Marshal(fields)
}
