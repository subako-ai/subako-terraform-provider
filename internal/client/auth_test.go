package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI writes a program standing in for the Subako CLI, which prints
// `output` for `token`, with every `%d` in it the number of times it has run,
// and answers the program and how to read that count.
func fakeCLI(t *testing.T, output string) (string, func() int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	template := filepath.Join(dir, "output")
	script := filepath.Join(dir, "subako")
	if err := os.WriteFile(template, []byte(output+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"[ \"$1\" = token ] || { echo \"unexpected arguments: $*\" >&2; exit 2; }\n" +
		"echo \"$*\" >> '" + filepath.Join(dir, "args") + "'\n" +
		"n=$(cat '" + count + "' 2>/dev/null || echo 0)\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > '" + count + "'\n" +
		"sed \"s/%d/$n/g\" '" + template + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	runs := func() int {
		raw, err := os.ReadFile(count)
		if err != nil {
			return 0
		}
		var n int
		for _, c := range strings.TrimSpace(string(raw)) {
			n = n*10 + int(c-'0')
		}
		return n
	}
	return script, runs
}

func TestStaticTellsAnAPIKeyFromAUserToken(t *testing.T) {
	for token, bound := range map[string]bool{"sbk_ak_x": true, "sbk_at_x": false} {
		auth, err := Static(token)
		if err != nil {
			t.Fatalf("%s: %v", token, err)
		}
		if auth.boundToAWorkspace() != bound {
			t.Errorf("%s: bound to a workspace = %v", token, !bound)
		}
	}
	for _, token := range []string{"", "abc"} {
		if _, err := Static(token); err == nil {
			t.Errorf("%q: Static succeeded", token)
		}
	}
}

func TestAskCLIReadsWhatTheCLIPrints(t *testing.T) {
	program, runs := fakeCLI(t, `{"version":1,"server":"https://api.example.test",`+
		`"access_token":"sbk_at_%d","expires_at":"2030-01-01T00:00:00.5Z",`+
		`"workspace_id":"ws-9","workspace_name":"infra"}`)

	issued, auth, err := AskCLI(context.Background(), program)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Server != "https://api.example.test" || issued.AccessToken != "sbk_at_1" ||
		issued.WorkspaceID != "ws-9" || issued.WorkspaceName != "infra" {
		t.Fatalf("issued = %+v", issued)
	}
	if want := time.Date(2030, 1, 1, 0, 0, 0, 5e8, time.UTC); !issued.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", issued.ExpiresAt, want)
	}
	if auth.boundToAWorkspace() {
		t.Fatal("a user's token names no workspace of its own")
	}
	// The token it was just handed has years left, so presenting it asks
	// nothing more of the CLI.
	if bearer, err := auth.bearer(context.Background()); err != nil || bearer != "sbk_at_1" {
		t.Fatalf("bearer = %q, %v", bearer, err)
	}
	if runs() != 1 {
		t.Fatalf("the CLI ran %d times", runs())
	}
}

// A long run outlives the token it started with: requests go on presenting it
// until it nears its expiry, then present the one the CLI answers next.
func TestTheCLIIsAskedAgainAsTheTokenNearsItsExpiry(t *testing.T) {
	program, runs := fakeCLI(t, `{"version":1,"server":"https://api.example.test",`+
		`"access_token":"sbk_at_%d","expires_at":"2030-01-01T00:00:00Z"}`)
	start := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	now := start
	auth := &cliAuth{
		program: program,
		now:     func() time.Time { return now },
		issued: Issued{
			Version:     issuedVersion,
			Server:      "https://api.example.test",
			AccessToken: "sbk_at_0",
			ExpiresAt:   start.Add(10 * time.Minute),
		},
	}

	var mu sync.Mutex
	var bearers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"a1"}`)
	}))
	defer server.Close()
	c, err := New(Config{Server: server.URL, Auth: auth, WorkspaceID: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}

	for _, at := range []time.Duration{0, 8 * time.Minute, 9*time.Minute + 30*time.Second, 9*time.Minute + 45*time.Second} {
		now = start.Add(at)
		if _, err := c.CreateAgent(context.Background(), "n"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Bearer sbk_at_0", "Bearer sbk_at_0", "Bearer sbk_at_1", "Bearer sbk_at_1"}
	if strings.Join(bearers, ",") != strings.Join(want, ",") {
		t.Fatalf("bearers = %v, want %v", bearers, want)
	}
	if runs() != 1 {
		t.Fatalf("the CLI ran %d times, once being the renewal", runs())
	}
}

func TestTheCLIAnsweringForAnotherServerMidRunIsRefused(t *testing.T) {
	program, _ := fakeCLI(t, `{"version":1,"server":"https://elsewhere.example.test",`+
		`"access_token":"sbk_at_%d","expires_at":"2030-01-01T00:00:00Z"}`)
	auth := &cliAuth{
		program: program,
		now:     time.Now,
		issued:  Issued{Server: "https://api.example.test", ExpiresAt: time.Now()},
	}
	_, err := auth.bearer(context.Background())
	if err == nil || !strings.Contains(err.Error(), "https://elsewhere.example.test") {
		t.Fatalf("err = %v", err)
	}
}

func TestACLIThatWillNotAnswerSaysWhy(t *testing.T) {
	failing, _ := fakeCLI(t, "")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'no profile selected: run subako login' >&2\nexit 1\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	newer, _ := fakeCLI(t, `{"version":2,"server":"https://api.example.test"}`)
	garbled, _ := fakeCLI(t, `not json`)

	for name, tc := range map[string]struct{ program, want string }{
		"missing":         {filepath.Join(t.TempDir(), "subako"), "subako login"},
		"missing on PATH": {"subako-not-installed-anywhere", "subako login"},
		"failing":         {failing, "no profile selected"},
		"newer output":    {newer, "version 2"},
		"garbled":         {garbled, "output"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := AskCLI(context.Background(), tc.program)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A refusal is answered by asking the CLI with --renew -- once, however many
// requests were refused together: the first renews, and the rest find the
// token they were refused is no longer the one in hand.
func TestARefusedTokenIsRenewedOnce(t *testing.T) {
	program, runs := fakeCLI(t, `{"version":1,"server":"https://api.example.test",`+
		`"access_token":"sbk_at_%d","expires_at":"2030-01-01T00:00:00Z"}`)
	auth := &cliAuth{
		program: program,
		now:     time.Now,
		issued: Issued{Server: "https://api.example.test", AccessToken: "sbk_at_0",
			ExpiresAt: time.Now().Add(time.Hour)},
	}

	for range 3 {
		renewed, err := auth.renew(context.Background(), "sbk_at_0")
		if err != nil || !renewed {
			t.Fatalf("renewed = %v, %v", renewed, err)
		}
	}
	if runs() != 1 || auth.issued.AccessToken != "sbk_at_1" {
		t.Fatalf("the CLI ran %d times, token %q", runs(), auth.issued.AccessToken)
	}
	args, err := os.ReadFile(filepath.Join(filepath.Dir(program), "args"))
	if err != nil || strings.TrimSpace(string(args)) != "token --renew" {
		t.Fatalf("the CLI was asked %q: %v", args, err)
	}

	static, _ := Static("sbk_ak_x")
	if renewed, _ := static.renew(context.Background(), "sbk_ak_x"); renewed {
		t.Fatal("a token the operator named has none to fall back on")
	}
}

// tokens is authentication that answers the next of its tokens each time a
// request asks, and renews to the one after.
type tokens struct {
	mu   sync.Mutex
	next []string
}

func (s *tokens) bearer(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.next[0]
	if len(s.next) > 1 {
		s.next = s.next[1:]
	}
	return token, nil
}

func (s *tokens) renew(context.Context, string) (bool, error) { return true, nil }

func (s *tokens) boundToAWorkspace() bool { return true }

// A request the server refuses is sent once more with the renewed token, and
// as the same operation: a POST keeps its idempotency key.
func TestARefusedRequestIsSentOnceMoreAsTheSameOperation(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer sbk_ak_old" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad token"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"a1"}`)
	}))
	defer server.Close()
	c, err := New(Config{Server: server.URL, Auth: &tokens{next: []string{"sbk_ak_old", "sbk_ak_new"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAgent(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[1].Get("Authorization") != "Bearer sbk_ak_new" {
		t.Fatalf("requests = %d, last bearer %q", len(seen), seen[len(seen)-1].Get("Authorization"))
	}
	if key := seen[0].Get("Idempotency-Key"); key == "" || seen[1].Get("Idempotency-Key") != key {
		t.Fatalf("keys = %q, %q", seen[0].Get("Idempotency-Key"), seen[1].Get("Idempotency-Key"))
	}
}

// A retry presents the token auth answers when it goes out, not the one its
// first attempt carried, which may have lapsed during the wait.
func TestARetryPresentsTheTokenOfItsOwnMoment(t *testing.T) {
	var mu sync.Mutex
	var bearers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		first := len(bearers) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"id":"a1"}`)
	}))
	defer server.Close()
	c, err := New(Config{Server: server.URL, Auth: &tokens{next: []string{"sbk_ak_1", "sbk_ak_2"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.retrying.RetryWaitMin, c.retrying.RetryWaitMax = 0, 0
	if _, err := c.CreateAgent(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bearer sbk_ak_1", "Bearer sbk_ak_2"}; strings.Join(bearers, ",") != strings.Join(want, ",") {
		t.Fatalf("bearers = %v, want %v", bearers, want)
	}
}
