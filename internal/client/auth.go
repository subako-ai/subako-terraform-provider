package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Auth answers the bearer a request presents. Its methods are
// unexported, so the two kinds below are the only ones there are.
type Auth interface {
	bearer(ctx context.Context) (string, error)
	// renew answers whether a request refused with `refused` is worth sending
	// again: true once a token other than `refused` is ready to present.
	renew(ctx context.Context, refused string) (bool, error)
	// boundToAWorkspace reports whether the server reads the workspace from
	// the token itself, as it does for an API key, so a request needs
	// none named beside it.
	boundToAWorkspace() bool
}

// staticAuth is a token the configuration or the environment named,
// presented unchanged for the life of the run.
type staticAuth struct {
	token  string
	apiKey bool
}

// Static checks a token's spelling and answers it as the run's authentication.
func Static(token string) (Auth, error) {
	switch {
	case token == "":
		return nil, errors.New("a token is required")
	case strings.HasPrefix(token, APIKeyMarker):
		return staticAuth{token: token, apiKey: true}, nil
	case strings.HasPrefix(token, AccessTokenMarker):
		return staticAuth{token: token}, nil
	default:
		return nil, errors.New("the token is neither an API key nor a user access token")
	}
}

func (s staticAuth) bearer(context.Context) (string, error) { return s.token, nil }

// A token the operator named has no other to fall back on: its refusal is
// the answer.
func (staticAuth) renew(context.Context, string) (bool, error) { return false, nil }

func (s staticAuth) boundToAWorkspace() bool { return s.apiKey }

// Issued is what the Subako CLI's `token` command prints: the signed-in
// user's server, an access token and when it lapses, and the workspace
// `subako workspace use` selected, when one is.
type Issued struct {
	Version       int       `json:"version"`
	Server        string    `json:"server"`
	AccessToken   string    `json:"access_token"`
	ExpiresAt     time.Time `json:"expires_at"`
	WorkspaceID   string    `json:"workspace_id"`
	WorkspaceName string    `json:"workspace_name"`
}

// The output version this provider reads. The CLI moves it when a field is
// removed or changes meaning, not when one is added.
const issuedVersion = 1

// renewalMargin is how little of a token's life may be left before the CLI
// is asked again. The CLI hands out tokens with minutes to spare, so each is
// presented for a while before this asks again.
const renewalMargin = time.Minute

// cliAuth is the signed-in user's access token, read from the Subako
// CLI and read again as it nears its expiry. The CLI keeps the login, so the
// provider never holds the refresh token, and never rotates it.
type cliAuth struct {
	program string
	now     func() time.Time
	// Terraform calls a provider concurrently, and one renewal serves them
	// all.
	mu     sync.Mutex
	issued Issued
}

// AskCLI runs `program token`, and answers what it printed with
// authentication that asks again as the token nears its expiry.
func AskCLI(ctx context.Context, program string) (Issued, Auth, error) {
	issued, err := runCLI(ctx, program, false)
	if err != nil {
		return Issued{}, nil, err
	}
	return issued, &cliAuth{program: program, now: time.Now, issued: issued}, nil
}

func (c *cliAuth) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issued.ExpiresAt.Sub(c.now()) > renewalMargin {
		return c.issued.AccessToken, nil
	}
	if err := c.ask(ctx, false); err != nil {
		return "", err
	}
	return c.issued.AccessToken, nil
}

// renew asks the CLI for a new token after the server refused `refused`,
// which its stored expiry can still call live after the grant was revoked or
// the clock moved. Requests refused together ask once: the first to get here
// renews, and the rest find a token other than the one they were refused.
func (c *cliAuth) renew(ctx context.Context, refused string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issued.AccessToken != refused {
		return true, nil
	}
	if err := c.ask(ctx, true); err != nil {
		return false, err
	}
	return true, nil
}

// ask runs the CLI and keeps what it answers. The caller holds c.mu.
func (c *cliAuth) ask(ctx context.Context, renew bool) error {
	issued, err := runCLI(ctx, c.program, renew)
	if err != nil {
		return err
	}
	// A token for another server would be refused there: the run was
	// configured against the one it started with.
	if issued.Server != c.issued.Server {
		return fmt.Errorf("the Subako CLI is signed in to %s now, not %s as when the run began",
			issued.Server, c.issued.Server)
	}
	c.issued = issued
	return nil
}

func (c *cliAuth) boundToAWorkspace() bool { return false }

// runCLI runs `program token`, with `--renew` when `renew` asks the CLI to
// mint a token whatever the stored one has left.
func runCLI(ctx context.Context, program string, renew bool) (Issued, error) {
	args := []string{"token"}
	if renew {
		args = append(args, "--renew")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return Issued{}, fmt.Errorf("%s was not found: install the Subako CLI and run `subako login`, "+
				"or set a token", program)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return Issued{}, fmt.Errorf("`%s token` failed: %s", program, detail)
	}
	var issued Issued
	if err := json.Unmarshal(stdout.Bytes(), &issued); err != nil {
		return Issued{}, fmt.Errorf("read `%s token`'s output: %w", program, err)
	}
	if issued.Version != issuedVersion {
		return Issued{}, fmt.Errorf("`%s token` printed version %d of its output, and this provider reads "+
			"version %d: install a Subako CLI and a provider released together", program, issued.Version, issuedVersion)
	}
	return issued, nil
}
