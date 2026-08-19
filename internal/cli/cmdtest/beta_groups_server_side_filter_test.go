package cmdtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func betaGroupsJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func runBetaGroupsList(t *testing.T, args ...string) (string, string) {
	t.Helper()

	root := RootCommand("1.2.3")
	root.FlagSet.SetOutput(io.Discard)

	stdout, stderr := captureOutput(t, func() {
		if err := root.Parse(append([]string{"testflight", "groups", "list"}, args...)); err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if err := root.Run(context.Background()); err != nil {
			t.Fatalf("run error: %v", err)
		}
	})
	return stdout, stderr
}

// TestBetaGroupsListAppScopedInternalFilterUsesTopLevelEndpoint proves the
// app-scoped internal/external filter is applied server-side through
// GET /v1/betaGroups instead of being post-filtered client-side.
func TestBetaGroupsListAppScopedInternalFilterUsesTopLevelEndpoint(t *testing.T) {
	setupAuth(t)
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	var requests atomic.Int64
	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", req.Method)
		}
		if req.URL.Path != "/v1/betaGroups" {
			t.Errorf("expected path /v1/betaGroups, got %s", req.URL.Path)
		}
		query := req.URL.Query()
		if got := query.Get("filter[app]"); got != "app-1" {
			t.Errorf("filter[app] = %q, want app-1", got)
		}
		if got := query.Get("filter[isInternalGroup]"); got != "true" {
			t.Errorf("filter[isInternalGroup] = %q, want true", got)
		}
		if got := query.Get("limit"); got != "" {
			t.Errorf("limit = %q, want no limit forced onto the request", got)
		}
		body := `{"data":[{"type":"betaGroups","id":"bg-int","attributes":{"name":"Internal","isInternalGroup":true}}],` +
			`"links":{"next":"https://api.appstoreconnect.apple.com/v1/betaGroups?cursor=page2"}}`
		return betaGroupsJSONResponse(body), nil
	}))

	stdout, stderr := runBetaGroupsList(t, "--app", "app-1", "--internal")

	if strings.Contains(stderr, "filtered groups") {
		t.Fatalf("expected no client-side truncation warning, got %q", stderr)
	}
	// Unfetched pages are surfaced by the standard pagination hint, not by a
	// truncation warning, because the command no longer walks every page.
	if !strings.Contains(stderr, "more pages exist") {
		t.Fatalf("expected the standard pagination hint, got %q", stderr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request without --paginate, got %d", got)
	}
	if !strings.Contains(stdout, `"id":"bg-int"`) {
		t.Fatalf("expected internal group in output, got %q", stdout)
	}
	// Apple's envelope must be printed unmodified, including links.next.
	if !strings.Contains(stdout, `"next":"https://api.appstoreconnect.apple.com/v1/betaGroups?cursor=page2"`) {
		t.Fatalf("expected links.next preserved in envelope, got %q", stdout)
	}
}

// TestBetaGroupsListAppScopedExternalFilterPassesLimitThrough proves --limit is
// forwarded as the page size instead of truncating a client-side filtered set.
func TestBetaGroupsListAppScopedExternalFilterPassesLimitThrough(t *testing.T) {
	setupAuth(t)
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	var requests atomic.Int64
	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.URL.Path != "/v1/betaGroups" {
			t.Errorf("expected path /v1/betaGroups, got %s", req.URL.Path)
		}
		query := req.URL.Query()
		if got := query.Get("filter[app]"); got != "app-1" {
			t.Errorf("filter[app] = %q, want app-1", got)
		}
		if got := query.Get("filter[isInternalGroup]"); got != "false" {
			t.Errorf("filter[isInternalGroup] = %q, want false", got)
		}
		if got := query.Get("limit"); got != "2" {
			t.Errorf("limit = %q, want 2", got)
		}
		body := `{"data":[` +
			`{"type":"betaGroups","id":"bg-ext-1","attributes":{"name":"External 1","isInternalGroup":false}},` +
			`{"type":"betaGroups","id":"bg-ext-2","attributes":{"name":"External 2","isInternalGroup":false}}` +
			`]}`
		return betaGroupsJSONResponse(body), nil
	}))

	stdout, stderr := runBetaGroupsList(t, "--app", "app-1", "--external", "--limit", "2")

	if stderr != "" {
		t.Fatalf("expected no truncation warning on stderr, got %q", stderr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("failed to parse json output: %v\noutput: %q", err, stdout)
	}
	if len(parsed.Data) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(parsed.Data))
	}
}

// TestBetaGroupsListAppScopedFilterPaginates proves --paginate still aggregates
// every page while filtering server-side.
func TestBetaGroupsListAppScopedFilterPaginates(t *testing.T) {
	setupAuth(t)
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	const nextURL = "https://api.appstoreconnect.apple.com/v1/betaGroups?cursor=page2"
	var requests atomic.Int64
	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		count := requests.Add(1)
		if req.URL.Path != "/v1/betaGroups" {
			t.Errorf("expected path /v1/betaGroups, got %s", req.URL.Path)
		}
		query := req.URL.Query()
		switch count {
		case 1:
			if got := query.Get("filter[app]"); got != "app-1" {
				t.Errorf("filter[app] = %q, want app-1", got)
			}
			if got := query.Get("filter[isInternalGroup]"); got != "true" {
				t.Errorf("filter[isInternalGroup] = %q, want true", got)
			}
			if got := query.Get("limit"); got != "200" {
				t.Errorf("limit = %q, want 200 for --paginate", got)
			}
			body := `{"data":[{"type":"betaGroups","id":"bg-int-1","attributes":{"name":"Internal 1","isInternalGroup":true}}],` +
				`"links":{"next":"` + nextURL + `"}}`
			return betaGroupsJSONResponse(body), nil
		case 2:
			if got := query.Get("cursor"); got != "page2" {
				t.Errorf("cursor = %q, want page2", got)
			}
			body := `{"data":[{"type":"betaGroups","id":"bg-int-2","attributes":{"name":"Internal 2","isInternalGroup":true}}]}`
			return betaGroupsJSONResponse(body), nil
		default:
			t.Errorf("unexpected request %d: %s", count, req.URL.String())
			return betaGroupsJSONResponse(`{"data":[]}`), nil
		}
	}))

	stdout, stderr := runBetaGroupsList(t, "--app", "app-1", "--internal", "--paginate")

	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected 2 requests with --paginate, got %d", got)
	}
	if !strings.Contains(stdout, `"id":"bg-int-1"`) || !strings.Contains(stdout, `"id":"bg-int-2"`) {
		t.Fatalf("expected both pages aggregated, got %q", stdout)
	}
}

// TestBetaGroupsListNextStillPagesWithoutQueryFlags proves a bare --next keeps
// following the cursor URL verbatim.
func TestBetaGroupsListNextStillPagesWithoutQueryFlags(t *testing.T) {
	setupAuth(t)
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	const nextURL = "https://api.appstoreconnect.apple.com/v1/betaGroups?cursor=page3&filter%5Bapp%5D=app-1&filter%5BisInternalGroup%5D=true"
	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != nextURL {
			t.Errorf("request URL = %q, want %q", req.URL.String(), nextURL)
		}
		return betaGroupsJSONResponse(`{"data":[]}`), nil
	}))

	stdout, stderr := runBetaGroupsList(t, "--next", nextURL)

	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, `"data":[]`) {
		t.Fatalf("expected empty data envelope, got %q", stdout)
	}
}

// TestBetaGroupsListNextRejectsQueryFlags proves query-shaping flags are not
// accepted and silently discarded by the cursor URL.
func TestBetaGroupsListNextRejectsQueryFlags(t *testing.T) {
	t.Setenv("ASC_BYPASS_KEYCHAIN", "1")
	t.Setenv("ASC_APP_ID", "")
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	const nextURL = "https://api.appstoreconnect.apple.com/v1/betaGroups?cursor=page3"
	tests := []struct {
		flag    []string
		wantErr string
	}{
		{[]string{"--internal"}, "--next cannot be combined with --internal"},
		{[]string{"--external"}, "--next cannot be combined with --external"},
		{[]string{"--name", "Beta Testers"}, "--next cannot be combined with --name"},
		{[]string{"--sort", "name"}, "--next cannot be combined with --sort"},
	}
	for _, test := range tests {
		args := append([]string{"testflight", "groups", "list", "--next", nextURL}, test.flag...)
		assertUsageExit(t, args, test.wantErr)
	}
}

// TestBetaGroupsListUnfilteredAppScopedPathUnchanged proves the plain
// app-scoped listing keeps using GET /v1/apps/{id}/betaGroups.
func TestBetaGroupsListUnfilteredAppScopedPathUnchanged(t *testing.T) {
	setupAuth(t)
	t.Setenv("ASC_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/apps/app-1/betaGroups" {
			t.Errorf("expected path /v1/apps/app-1/betaGroups, got %s", req.URL.Path)
		}
		query := req.URL.Query()
		if got := query.Get("limit"); got != "5" {
			t.Errorf("limit = %q, want 5", got)
		}
		if got := query.Get("filter[app]"); got != "" {
			t.Errorf("filter[app] = %q, want none on the relationship endpoint", got)
		}
		if got := query.Get("filter[isInternalGroup]"); got != "" {
			t.Errorf("filter[isInternalGroup] = %q, want none", got)
		}
		if got := query.Get("sort"); got != "" {
			t.Errorf("sort = %q, want none", got)
		}
		return betaGroupsJSONResponse(`{"data":[{"type":"betaGroups","id":"bg-scoped","attributes":{"name":"Scoped"}}]}`), nil
	}))

	stdout, stderr := runBetaGroupsList(t, "--app", "app-1", "--limit", "5")

	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, `"id":"bg-scoped"`) {
		t.Fatalf("expected scoped group in output, got %q", stdout)
	}
}
