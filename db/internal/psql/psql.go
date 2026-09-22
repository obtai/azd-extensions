// Package psql mints the Entra token a flexible server accepts as a password,
// spells the connection, and hands the session to psql.
package psql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// Scope is the audience a flexible server checks the token against. It is not
// the ARM scope: a token for ARM is refused at the door.
const Scope = "https://ossrdbms-aad.database.windows.net/.default"

// Port is not configurable: a flexible server has no other one.
const Port = 5432

// Connection is everything psql needs.
type Connection struct {
	Host     string
	Database string
	User     string
	Token    string
}

// Token asks the credential for a token the server will accept.
func Token(ctx context.Context, credential azcore.TokenCredential, tenant string) (string, error) {
	options := policy.TokenRequestOptions{Scopes: []string{Scope}}
	// "common" is a placeholder, not a tenant; passing it on is a 400.
	if tenant != "" && tenant != "common" {
		options.TenantID = tenant
	}
	token, err := credential.GetToken(ctx, options)
	if err != nil {
		return "", fmt.Errorf("getting a database token: %w", err)
	}
	return token.Token, nil
}

// User reads the role name out of the token. The claim is not verified — the
// server does that — and nothing here trusts it beyond spelling a name.
func User(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", errors.New("the database token is not a JWT; pass --user")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("reading the database token's claims: %w", err)
	}
	var claims struct {
		UPN               string `json:"upn"`
		UniqueName        string `json:"unique_name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("reading the database token's claims: %w", err)
	}
	for _, name := range []string{claims.UPN, claims.UniqueName, claims.PreferredUsername} {
		if name != "" {
			return name, nil
		}
	}
	// A managed identity's token names nobody: its Postgres role is the
	// identity's own name, which only the deployment knows.
	return "", errors.New("the database token carries no user name; pass --user " +
		"(for a managed identity it is the identity's name, the APP_DATABASE_ROLE output)")
}

// KeywordString is the libpq connection string, the form that needs no
// escaping on the way to psql. The password travels in PGPASSWORD.
func (c Connection) KeywordString() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=require",
		c.Host, Port, c.Database, c.User)
}

// URL is the same connection for something that is not psql. It carries the
// token, so it is as secret as the token and lasts about as long.
func (c Connection) URL() string {
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(c.User, c.Token),
		Host:     fmt.Sprintf("%s:%d", c.Host, Port),
		Path:     "/" + c.Database,
		RawQuery: "sslmode=require",
	}
	return u.String()
}

// Args is psql's argument list: the connection, then -c, then whatever was
// passed after --.
func Args(connection string, command string, passthrough []string) []string {
	args := []string{connection}
	if command != "" {
		args = append(args, "-c", command)
	}
	return append(args, passthrough...)
}

// Run hands the terminal to psql and returns its exit status.
func (c Connection) Run(ctx context.Context, command string, passthrough []string) error {
	path, err := exec.LookPath("psql")
	if err != nil {
		return errors.New("psql is not on PATH. Install the PostgreSQL client " +
			"(brew install libpq, apt install postgresql-client), or use " +
			"--url to hand the connection to another client")
	}

	cmd := exec.CommandContext(ctx, path, Args(c.KeywordString(), command, passthrough)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The token is a password: PGPASSWORD keeps it off the command line, where
	// every other process on the machine could read it.
	cmd.Env = append(os.Environ(), "PGPASSWORD="+c.Token)

	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// psql has already said why; passing its status on keeps
		// `azd db -c … && …` honest.
		return &ExitError{Code: exit.ExitCode()}
	}
	return err
}

// ExitError carries psql's own exit status out to main.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("psql exited with status %d", e.Code) }
