package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
)

// Registry bounds (§23.6).
const (
	requestTimeout   = 20 * time.Second
	maxResponseBytes = 4 << 20
	maxTokenBytes    = 64 << 10
	maxHeaderBytes   = 8 << 10
	maxRedirects     = 3
	retryAttempts    = 4
)

var manifestAccept = strings.Join([]string{
	descriptor.MediaTypeIndex,
	"application/vnd.docker.distribution.manifest.list.v2+json",
	descriptor.MediaTypeManifest,
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// errNotFound is a registry 404: the tag, manifest or blob does not exist.
var errNotFound = errors.New("not found")

// statusError is an unexpected registry status. It never carries a response body.
type statusError struct{ code int }

func (e *statusError) Error() string { return "registry status " + http.StatusText(e.code) }

// retryable reports transient failures: transport errors, 408, 429 and 5xx.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusRequestTimeout || se.code == http.StatusTooManyRequests || se.code >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Registry is a minimal OCI distribution client for the publisher. It authenticates only to its own origin, follows
// a bearer challenge only to a realm on that origin, never logs, and never puts a credential or response body in an
// error. Every request has its own deadline and bounded reads.
type Registry struct {
	Base     string // https://<host>, or http://127.0.0.1:<port> for a local test registry
	Username string
	Password string
	HTTP     *http.Client
	Sleep    func(context.Context, time.Duration) error // backoff; tests make it instant

	mu        sync.Mutex
	tokens    map[string]string // repository → push/pull token
	transport http.RoundTripper // the default transport, built once when HTTP is nil
}

func (r *Registry) client(blob bool) *http.Client {
	c := http.Client{}
	if r.HTTP != nil {
		c = *r.HTTP
	} else {
		r.mu.Lock()
		if r.transport == nil {
			r.transport = &http.Transport{
				Proxy:                  http.ProxyFromEnvironment,
				MaxResponseHeaderBytes: maxHeaderBytes,
				TLSHandshakeTimeout:    10 * time.Second,
				ResponseHeaderTimeout:  requestTimeout,
			}
		}
		c.Transport = r.transport
		r.mu.Unlock()
	}
	c.Jar = nil
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// D-047: credentials never follow a redirect, and only blob reads may leave the registry origin.
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		if len(via) > maxRedirects || req.URL.User != nil || (!blob && !r.sameOrigin(req.URL)) ||
			(blob && req.URL.Scheme != "https" && !r.sameOrigin(req.URL)) {
			return errors.New("redirect refused")
		}
		return nil
	}
	return &c
}

func (r *Registry) origin() *url.URL {
	u, err := url.Parse(r.Base)
	if err != nil {
		return &url.URL{}
	}
	return u
}

func (r *Registry) sameOrigin(u *url.URL) bool {
	o := r.origin()
	return u.Scheme == o.Scheme && u.Host == o.Host
}

func (r *Registry) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// response is a bounded registry response.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request with the repository's token, refreshing it once on 401. The body is read up to limit.
func (r *Registry) do(ctx context.Context, repo, method, target string, header http.Header, body []byte, limit int64, blob bool) (response, error) {
	u, err := url.Parse(target)
	if err != nil || !r.sameOrigin(u) {
		return response{}, errors.New("request target outside the registry")
	}
	for attempt := 0; ; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		req, err := http.NewRequestWithContext(reqCtx, method, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			return response{}, errors.New("build request")
		}
		for k, v := range header {
			req.Header[k] = v
		}
		if body == nil {
			req.Body = http.NoBody
		}
		if token := r.cachedToken(repo); token != "" {
			req.Header.Set("Authorization", token)
		}
		resp, err := r.client(blob).Do(req)
		if err != nil {
			cancel()
			return response{}, safeError(err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close() // read-only; ReadAll reports read failures
		cancel()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if err := r.authenticate(ctx, repo, resp.Header.Get("WWW-Authenticate")); err != nil {
				return response{}, err
			}
			continue
		}
		if readErr != nil {
			return response{}, safeError(readErr)
		}
		if int64(len(data)) > limit {
			return response{}, fail(CodeRegistryInvalid, "response size")
		}
		return response{status: resp.StatusCode, header: resp.Header, body: data}, nil
	}
}

// safeError drops URLs and transport text, which may contain signed query strings, keeping only the error class.
func safeError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return &net.OpError{Op: "registry", Err: errors.New("transport failure")}
	}
	return io.ErrUnexpectedEOF
}

func (r *Registry) cachedToken(repo string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[repo]
}

func (r *Registry) setToken(repo, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tokens == nil {
		r.tokens = map[string]string{}
	}
	r.tokens[repo] = value
}

// authenticate answers a 401 challenge. Basic challenges get the configured credential. A bearer challenge is
// followed only to a realm on the registry's own origin (§23.6), with a push/pull scope for exactly this repository.
func (r *Registry) authenticate(ctx context.Context, repo, challenge string) error {
	if len(challenge) > maxHeaderBytes {
		return fail(CodeRegistryInvalid, "challenge")
	}
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if r.Password == "" {
			return fail(CodeCredentialInvalid, "")
		}
		r.setToken(repo, "Basic "+base64.StdEncoding.EncodeToString([]byte(r.Username+":"+r.Password)))
		return nil
	case "bearer":
	default:
		return fail(CodeRegistryInvalid, "challenge")
	}
	p := challengeParams(params)
	realm, err := url.Parse(p["realm"])
	if err != nil || !r.sameOrigin(realm) || realm.User != nil || realm.RawQuery != "" || realm.Fragment != "" {
		return fail(CodeRegistryInvalid, "challenge")
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	q.Set("scope", "repository:"+repo+":pull,push")
	realm.RawQuery = q.Encode()
	for attempt := 0; ; attempt++ {
		token, err := r.fetchToken(ctx, realm.String())
		if err == nil {
			r.setToken(repo, "Bearer "+token)
			return nil
		}
		if attempt+1 >= retryAttempts || !retryable(err) {
			return err
		}
		if err := r.sleep(ctx, backoff(attempt)); err != nil {
			return failWith(CodeRegistryUnavailable, "token", err)
		}
	}
}

func (r *Registry) fetchToken(ctx context.Context, target string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return "", errors.New("build request")
	}
	if r.Password != "" {
		req.SetBasicAuth(r.Username, r.Password)
	}
	resp, err := r.client(false).Do(req)
	if err != nil {
		return "", safeError(err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fail(CodeCredentialInvalid, "")
	case resp.StatusCode != http.StatusOK:
		return "", &statusError{code: resp.StatusCode}
	case err != nil:
		return "", safeError(err)
	case len(data) > maxTokenBytes:
		return "", fail(CodeRegistryInvalid, "token")
	}
	v, err := descriptor.Parse(data, descriptor.Limits{Bytes: maxTokenBytes, Depth: 4, Nodes: 64})
	if err != nil || v.Kind != descriptor.Object {
		return "", fail(CodeRegistryInvalid, "token")
	}
	var token string
	for _, m := range v.Members {
		if (m.Key == "token" || (m.Key == "access_token" && token == "")) && m.Value.Kind == descriptor.String {
			token = m.Value.Str
		}
	}
	if token == "" || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n ") {
		return "", fail(CodeRegistryInvalid, "token")
	}
	return token, nil
}

// challengeParams parses the comma-separated key="value" list of a bearer challenge.
func challengeParams(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, ", ")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				break
			}
			value, s = rest[1:end+1], rest[end+2:]
		} else {
			value, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return out
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 500 * time.Millisecond
	return base + rand.N(base/2+1) //nolint:gosec // G404: backoff jitter needs no cryptographic randomness
}

// retry runs op until it succeeds, fails permanently or exhausts its attempts.
func (r *Registry) retry(ctx context.Context, op func() error) error {
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil || !retryable(err) || attempt+1 >= retryAttempts {
			return err
		}
		if err := r.sleep(ctx, backoff(attempt)); err != nil {
			return err
		}
	}
}

func (r *Registry) url(repo, kind, ref string) string {
	return strings.TrimRight(r.Base, "/") + "/v2/" + repo + "/" + kind + "/" + ref
}

// classify maps a response status to an error.
func classify(resp response, ok ...int) error {
	for _, code := range ok {
		if resp.status == code {
			return nil
		}
	}
	switch resp.status {
	case http.StatusNotFound:
		return errNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return fail(CodeWriteAccessDenied, "")
	}
	return &statusError{code: resp.status}
}

// Manifest is a manifest read by reference: its exact bytes, media type and computed digest.
type Manifest struct {
	Body      []byte
	MediaType string
	Digest    string
}

// GetManifest reads a manifest by tag or digest, bypassing caches, and verifies a reported digest against the bytes.
func (r *Registry) GetManifest(ctx context.Context, repo, ref string) (Manifest, error) {
	var m Manifest
	err := r.retry(ctx, func() error {
		resp, err := r.do(ctx, repo, http.MethodGet, r.url(repo, "manifests", ref),
			http.Header{"Accept": {manifestAccept}, "Cache-Control": {"no-cache"}}, nil, maxResponseBytes, false)
		if err != nil {
			return err
		}
		if err := classify(resp, http.StatusOK); err != nil {
			return err
		}
		m.Body = resp.body
		m.Digest = descriptor.SHA256(resp.body)
		ct, _, _ := strings.Cut(resp.header.Get("Content-Type"), ";")
		m.MediaType = strings.TrimSpace(ct)
		if h := resp.header.Get("Docker-Content-Digest"); h != "" && h != m.Digest {
			return fail(CodeRegistryInvalid, "manifest digest")
		}
		if descriptor.Digest(ref) && ref != m.Digest {
			return fail(CodeRegistryInvalid, "manifest digest")
		}
		return nil
	})
	return m, err
}

// PutManifest writes manifest bytes under ref and checks a reported digest, retrying transient failures. Only
// idempotent writes use it; immutable tags and the head use putOnce with readback reconciliation.
func (r *Registry) PutManifest(ctx context.Context, repo, ref, mediaType string, body []byte) error {
	return r.retry(ctx, func() error { return r.putOnce(ctx, repo, ref, mediaType, body) })
}

// putOnce sends one manifest PUT. A transport error or 5xx leaves the write ambiguous: the caller reads back.
func (r *Registry) putOnce(ctx context.Context, repo, ref, mediaType string, body []byte) error {
	resp, err := r.do(ctx, repo, http.MethodPut, r.url(repo, "manifests", ref),
		http.Header{"Content-Type": {mediaType}}, body, maxTokenBytes, false)
	if err != nil {
		return err
	}
	if err := classify(resp, http.StatusCreated, http.StatusOK); err != nil {
		return err
	}
	if h := resp.header.Get("Docker-Content-Digest"); h != "" && h != descriptor.SHA256(body) {
		return fail(CodeRegistryInvalid, "manifest digest")
	}
	return nil
}

// BlobExists reports whether repo holds a blob.
func (r *Registry) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	var exists bool
	err := r.retry(ctx, func() error {
		resp, err := r.do(ctx, repo, http.MethodHead, r.url(repo, "blobs", digest), nil, nil, 0, true)
		if err != nil {
			return err
		}
		err = classify(resp, http.StatusOK)
		exists = err == nil
		if errors.Is(err, errNotFound) {
			return nil
		}
		return err
	})
	return exists, err
}

// GetBlob reads a blob of at most limit bytes and verifies it against its digest (D-047).
func (r *Registry) GetBlob(ctx context.Context, repo, digest string, limit int64) ([]byte, error) {
	var body []byte
	err := r.retry(ctx, func() error {
		resp, err := r.do(ctx, repo, http.MethodGet, r.url(repo, "blobs", digest), nil, nil, limit, true)
		if err != nil {
			return err
		}
		if err := classify(resp, http.StatusOK); err != nil {
			return err
		}
		if descriptor.SHA256(resp.body) != digest {
			return fail(CodeRegistryInvalid, "blob digest")
		}
		body = resp.body
		return nil
	})
	return body, err
}

// PutBlob uploads a blob unless the repository already has it: a POST opens an upload session, and a single PUT
// to its same-origin Location completes it with the digest.
func (r *Registry) PutBlob(ctx context.Context, repo string, body []byte) error {
	digest := descriptor.SHA256(body)
	exists, err := r.BlobExists(ctx, repo, digest)
	if err != nil || exists {
		return err
	}
	return r.retry(ctx, func() error {
		location, err := r.startUpload(ctx, repo)
		if err != nil {
			return err
		}
		q := location.Query()
		q.Set("digest", digest)
		location.RawQuery = q.Encode()
		resp, err := r.do(ctx, repo, http.MethodPut, location.String(),
			http.Header{"Content-Type": {"application/octet-stream"}}, body, maxTokenBytes, false)
		if err != nil {
			return err
		}
		return classify(resp, http.StatusCreated, http.StatusOK, http.StatusNoContent)
	})
}

func (r *Registry) startUpload(ctx context.Context, repo string) (*url.URL, error) {
	resp, err := r.do(ctx, repo, http.MethodPost, r.url(repo, "blobs", "uploads/"), nil, nil, maxTokenBytes, false)
	if err != nil {
		return nil, err
	}
	if err := classify(resp, http.StatusAccepted); err != nil {
		return nil, err
	}
	raw := resp.header.Get("Location")
	if raw == "" || len(raw) > maxHeaderBytes {
		return nil, fail(CodeRegistryInvalid, "upload location")
	}
	loc, err := r.origin().Parse(raw)
	if err != nil || !r.sameOrigin(loc) || loc.User != nil || loc.Fragment != "" {
		return nil, fail(CodeRegistryInvalid, "upload location")
	}
	return loc, nil
}

// ProbeWrite checks write access to repo before any content is uploaded (§23.2 step 1): it opens an upload session
// and cancels it. Nothing is committed.
func (r *Registry) ProbeWrite(ctx context.Context, repo string) error {
	return r.retry(ctx, func() error {
		loc, err := r.startUpload(ctx, repo)
		if errors.Is(err, errNotFound) {
			return fail(CodeWriteAccessDenied, "")
		}
		if err != nil {
			return err
		}
		// Best effort: registries may expire the session instead of supporting DELETE.
		_, _ = r.do(ctx, repo, http.MethodDelete, loc.String(), nil, nil, maxTokenBytes, false)
		return nil
	})
}

// Referrer bounds (§23.6): pages per listing and entries across them.
const (
	maxReferrerPages   = 100
	maxReferrerEntries = 10000
)

// Referrers lists the digests of a subject's referrers of artifactType through the referrers API, following
// Link-header pagination on the registry origin and the same route, with bounded pages and entries. supported is
// false on an explicit 404 or 405 of the first page, the only capability signals (§23.2); other failures, invalid
// pages, foreign or looping links and exceeded bounds are errors.
func (r *Registry) Referrers(ctx context.Context, repo, digest, artifactType string) (digests []string, supported bool, err error) {
	first := r.url(repo, "referrers", digest) + "?artifactType=" + url.QueryEscape(artifactType)
	route, err := url.Parse(first)
	if err != nil {
		return nil, false, fail(CodeRegistryInvalid, "referrers")
	}
	seen := map[string]bool{}
	for page, target := 0, first; target != ""; page++ {
		if page >= maxReferrerPages || seen[target] {
			return nil, false, fail(CodeRegistryInvalid, "referrers pagination")
		}
		seen[target] = true
		var body []byte
		var link string
		err := r.retry(ctx, func() error {
			resp, err := r.do(ctx, repo, http.MethodGet, target, http.Header{"Accept": {descriptor.MediaTypeIndex}}, nil, maxResponseBytes, false)
			if err != nil {
				return err
			}
			switch resp.status {
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				if page == 0 {
					body = nil
					return nil
				}
			case http.StatusOK:
				ct, _, _ := strings.Cut(resp.header.Get("Content-Type"), ";")
				if strings.TrimSpace(ct) != descriptor.MediaTypeIndex {
					return fail(CodeRegistryInvalid, "referrers") // an error, never a capability signal (§23.2)
				}
				body, link = resp.body, resp.header.Get("Link")
				if body == nil {
					body = []byte{}
				}
				return nil
			}
			return classify(resp)
		})
		if err != nil {
			return nil, false, err
		}
		if body == nil {
			return nil, false, nil
		}
		v, ok := parseGraph(body)
		manifests := get(v, "manifests")
		if !ok || manifests.Kind != descriptor.Array {
			return nil, false, fail(CodeRegistryInvalid, "referrers")
		}
		for _, m := range manifests.Items {
			if d := get(m, "digest"); d.Kind == descriptor.String && descriptor.Digest(d.Str) {
				digests = append(digests, d.Str)
			}
		}
		if len(digests) > maxReferrerEntries {
			return nil, false, fail(CodeRegistryInvalid, "referrers entries")
		}
		if target, err = nextPage(route, target, link); err != nil {
			return nil, false, err
		}
	}
	return digests, true, nil
}

// nextPage resolves a Link header's rel="next" target against the current page. It must stay on the registry origin
// and the referrers route of the first page; an absent link ends the listing.
func nextPage(route *url.URL, current, link string) (string, error) {
	if link == "" {
		return "", nil
	}
	if len(link) > maxHeaderBytes {
		return "", fail(CodeRegistryInvalid, "referrers pagination")
	}
	for _, part := range strings.Split(link, ",") {
		ref, params, found := strings.Cut(strings.TrimSpace(part), ";")
		if !found || !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) ||
			!strings.HasPrefix(ref, "<") || !strings.HasSuffix(ref, ">") {
			continue
		}
		base, err := url.Parse(current)
		if err != nil {
			return "", fail(CodeRegistryInvalid, "referrers pagination")
		}
		next, err := base.Parse(strings.TrimSuffix(strings.TrimPrefix(ref, "<"), ">"))
		if err != nil || next.Scheme != route.Scheme || next.Host != route.Host || next.Path != route.Path ||
			next.User != nil || next.Fragment != "" || strings.ContainsAny(next.String(), " \t\r\n") {
			return "", fail(CodeRegistryInvalid, "referrers pagination")
		}
		return next.String(), nil
	}
	return "", nil
}
