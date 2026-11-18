package source

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wzshiming/httpcache"
)

const (
	reqBody      = `{"query":"q"}`
	okBody       = `{"data":{"user":{"pullRequests":{"totalCount":1}}}}`
	timeoutBody  = "{\"data\":null,\"errors\":[{\"message\":\"Something went wrong while executing your query on 2026-11-17T10:08:13Z. Please include `7480:2FE9D1:14B9091:4229672:6AFC2789` when reporting this issue.\"}]}"
	notFoundBody = `{"data":{"user":null},"errors":[{"type":"NOT_FOUND","path":["user"],"message":"Could not resolve to a User with the login of 'ty-dc'."}]}`
)

// newTestClient builds the same transport chain as NewSource does for the
// GraphQL client: httpcache -> graphqlRetry -> intervalRequest -> network.
func newTestClient(retry int) *http.Client {
	interval := time.Millisecond
	transport := newIntervalRequest(http.DefaultTransport, interval, retry)
	return &http.Client{
		Transport: httpcache.NewRoundTripper(newGraphQLRetry(transport, interval, retry),
			httpcache.WithFilterer(
				httpcache.MethodFilterer(http.MethodPost),
			),
			httpcache.WithKeyer(
				httpcache.JointKeyer(
					httpcache.MethodKeyer(),
					httpcache.PathKeyer(),
					httpcache.BodyKeyer(),
				),
			),
		),
	}
}

// newTestServer answers with replies in order and repeats the last one.
func newTestServer(t *testing.T, replies ...string) (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != reqBody {
			t.Errorf("request body = %q, want %q (must be replayed unchanged)", body, reqBody)
		}
		n := int(atomic.AddInt32(&hits, 1))
		if n > len(replies) {
			n = len(replies)
		}
		io.WriteString(w, replies[n-1])
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func post(t *testing.T, cli *http.Client, url string) string {
	t.Helper()
	resp, err := cli.Post(url+"/graphql", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestGraphQLRetry(t *testing.T) {
	const retry = 3
	tests := []struct {
		name     string
		replies  []string
		want     string
		wantHits int32
	}{
		{"ok", []string{okBody}, okBody, 1},
		{"server timeout then ok", []string{timeoutBody, okBody}, okBody, 2},
		{"empty body then ok", []string{"", okBody}, okBody, 2},
		{"typed error is not retried", []string{notFoundBody, okBody}, notFoundBody, 1},
		{"persistent timeout gives up after retries", []string{timeoutBody}, timeoutBody, retry + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := newTestServer(t, tt.replies...)
			cli := newTestClient(retry)

			if got := post(t, cli, srv.URL); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if got := atomic.LoadInt32(hits); got != tt.wantHits {
				t.Fatalf("server hits = %d, want %d", got, tt.wantHits)
			}

			// The next file asks for the same user and state: it must be
			// answered from the cache without touching the server.
			if got := post(t, cli, srv.URL); got != tt.want {
				t.Fatalf("second request: got %q, want %q", got, tt.want)
			}
			if got := atomic.LoadInt32(hits); got != tt.wantHits {
				t.Fatalf("server hits after second request = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestIsTransientGraphQLError(t *testing.T) {
	tests := map[string]bool{
		okBody:       false,
		notFoundBody: false,
		`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`: false,
		timeoutBody: true,
		"":          true,
		`{"data":`:  true,
	}
	for body, want := range tests {
		if got := isTransientGraphQLError([]byte(body)); got != want {
			t.Errorf("isTransientGraphQLError(%q) = %v, want %v", body, got, want)
		}
	}
}
