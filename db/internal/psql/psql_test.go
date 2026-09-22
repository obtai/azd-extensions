package psql

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// token builds a JWT with the claims a test cares about. Only the payload is
// ever read, so the other two segments are filler.
func token(claims map[string]string) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestUser(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]string
		want   string
	}{
		{"upn first", map[string]string{"upn": "ed@obtai.co.uk", "unique_name": "other"}, "ed@obtai.co.uk"},
		{"unique_name next", map[string]string{"unique_name": "ed@obtai.co.uk"}, "ed@obtai.co.uk"},
		{"preferred_username last", map[string]string{"preferred_username": "ed@obtai.co.uk"}, "ed@obtai.co.uk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := User(token(test.claims))
			if err != nil {
				t.Fatalf("User() error = %v", err)
			}
			if got != test.want {
				t.Errorf("User() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUserWithoutAName(t *testing.T) {
	// A managed identity's token names nobody; the error has to say what to pass.
	_, err := User(token(map[string]string{"oid": "ed73f61a"}))
	if err == nil {
		t.Fatal("User() with no name claim returned no error")
	}
	if !strings.Contains(err.Error(), "--user") {
		t.Errorf("User() error = %q, want it to name --user", err)
	}
}

func TestUserOnSomethingThatIsNotAToken(t *testing.T) {
	if _, err := User("not-a-jwt"); err == nil {
		t.Error("User() on a non-JWT returned no error")
	}
}

func TestKeywordString(t *testing.T) {
	c := Connection{Host: "psql-uks-dev-grid.postgres.database.azure.com", Database: "grid", User: "ed@obtai.co.uk"}
	want := "host=psql-uks-dev-grid.postgres.database.azure.com port=5432 dbname=grid user=ed@obtai.co.uk sslmode=require"
	if got := c.KeywordString(); got != want {
		t.Errorf("KeywordString() = %q, want %q", got, want)
	}
}

func TestURLEscapesTheUser(t *testing.T) {
	c := Connection{
		Host:     "psql-uks-dev-grid.postgres.database.azure.com",
		Database: "grid",
		User:     "ed+test@obtai.co.uk",
		Token:    "a.b.c",
	}
	got := c.URL()
	// `+` is legal unencoded in userinfo and libpq percent-decodes rather than
	// form-decodes, so it stays as it is; `@` has to go.
	want := "postgresql://ed+test%40obtai.co.uk:a.b.c@psql-uks-dev-grid.postgres.database.azure.com:5432/grid?sslmode=require"
	if got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestArgs(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		passthrough []string
		want        []string
	}{
		{"bare", "", nil, []string{"conn"}},
		{"command", "select 1", nil, []string{"conn", "-c", "select 1"}},
		{"passthrough", "", []string{"-f", "migrate.sql", "--csv"}, []string{"conn", "-f", "migrate.sql", "--csv"}},
		{"both", "select 1", []string{"--csv"}, []string{"conn", "-c", "select 1", "--csv"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Args("conn", test.command, test.passthrough)
			if !slices.Equal(got, test.want) {
				t.Errorf("Args() = %v, want %v", got, test.want)
			}
		})
	}
}
