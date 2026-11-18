package source

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

// graphqlRetry retries GraphQL responses that GitHub returns with HTTP 200
// but that carry a transient error: "Something went wrong while executing
// your query" (a server-side timeout, reported without an error type) or an
// empty or truncated body.
//
// It must sit between httpcache and intervalRequest. httpcache only drops
// 5xx responses, so without this such an error is stored on the first
// request and replayed for the rest of the run: every later file that needs
// the same user and state fails with the same request ID.
//
// If the error is still there after the retries, the response is returned
// unchanged and gets cached, which keeps the cost of a persistent failure
// bounded to one round of retries per run.
type graphqlRetry struct {
	next     http.RoundTripper
	retry    int
	interval time.Duration
}

func newGraphQLRetry(next http.RoundTripper, interval time.Duration, retry int) http.RoundTripper {
	if retry <= 0 {
		return next
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &graphqlRetry{
		next:     next,
		retry:    retry,
		interval: interval,
	}
}

func (g *graphqlRetry) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.Body == nil {
		return g.next.RoundTrip(r)
	}

	reqBody, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}

	for i := 0; ; i++ {
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(reqBody)), nil
		}
		req.ContentLength = int64(len(reqBody))

		resp, err := g.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusOK || i >= g.retry {
			return resp, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil && !isTransientGraphQLError(body) {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp, nil
		}

		log.Printf("transient GraphQL error, retry %d/%d: %s", i+1, g.retry, describeGraphQLFailure(body, err))
		time.Sleep(g.interval * time.Duration(i+1))
	}
}

// isTransientGraphQLError reports whether a 200 response from the GraphQL
// endpoint is worth retrying.
func isTransientGraphQLError(body []byte) bool {
	var out struct {
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		// Empty or truncated body.
		return true
	}
	for _, e := range out.Errors {
		// Typed errors (NOT_FOUND for a renamed or deleted user, FORBIDDEN,
		// RATE_LIMITED, ...) do not go away a few seconds later, and they
		// are cheap to cache. Server-side timeouts come without a type.
		if e.Type == "" {
			return true
		}
	}
	return false
}

func describeGraphQLFailure(body []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "empty body"
	}
	const limit = 200
	if len(body) > limit {
		return string(body[:limit]) + "..."
	}
	return string(body)
}
