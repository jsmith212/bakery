package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/config"
)

// TestRevokePurge drives `key revoke`, `token revoke` and `org robot revoke`
// through the fake server for both states of --purge: the request carries
// purge=true in the query iff the flag was set (and none at all otherwise --
// TestClientVerbs' base "key revoke" case already covers that half for the
// key command; this asserts it directly for all three), and a successful
// purge prints "deleted" where a plain revoke prints "revoked" (see
// keyRevoke/tokenRevoke/orgRobotRevoke in commands.go).
func TestRevokePurge(t *testing.T) {
	tests := []struct {
		name string

		purge bool
		run   func(ctx context.Context, c *Client, out *bytes.Buffer) error

		wantQuery string
		wantWord  string
	}{
		{
			name:  "key revoke without --purge",
			purge: false,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return keyRevoke(ctx, c, out, config.KeyRevokeCmd{Org: "acme", Project: "widgets", Key: "k1"})
			},
			wantQuery: "",
			wantWord:  "revoked key k1",
		},
		{
			name:  "key revoke --purge",
			purge: true,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return keyRevoke(ctx, c, out,
					config.KeyRevokeCmd{Org: "acme", Project: "widgets", Key: "k1", Purge: true})
			},
			wantQuery: "purge=true",
			wantWord:  "deleted key k1",
		},
		{
			name:  "token revoke without --purge",
			purge: false,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return tokenRevoke(ctx, c, out, config.TokenRevokeCmd{Token: "t1"})
			},
			wantQuery: "",
			wantWord:  "revoked token t1",
		},
		{
			name:  "token revoke --purge",
			purge: true,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return tokenRevoke(ctx, c, out, config.TokenRevokeCmd{Token: "t1", Purge: true})
			},
			wantQuery: "purge=true",
			wantWord:  "deleted token t1",
		},
		{
			name:  "org robot revoke without --purge",
			purge: false,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return orgRobotRevoke(ctx, c, out,
					config.OrgRobotRevokeCmd{Org: "acme", Robot: "r1", Token: "t1"})
			},
			wantQuery: "",
			wantWord:  "revoked token t1 on robot r1",
		},
		{
			name:  "org robot revoke --purge",
			purge: true,
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return orgRobotRevoke(ctx, c, out,
					config.OrgRobotRevokeCmd{Org: "acme", Robot: "r1", Token: "t1", Purge: true})
			},
			wantQuery: "purge=true",
			wantWord:  "deleted token t1 on robot r1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusNoContent)

				return true
			}

			c := f.client(t)

			var out bytes.Buffer

			if err := tc.run(t.Context(), c, &out); err != nil {
				t.Fatalf("call: %v", err)
			}

			if got := f.last().rawQuery; got != tc.wantQuery {
				t.Errorf("query = %q, want %q", got, tc.wantQuery)
			}

			if got := strings.TrimSpace(out.String()); got != tc.wantWord {
				t.Errorf("output = %q, want %q", got, tc.wantWord)
			}
		})
	}
}

// TestRevokePurgeRefusesALiveCredential: the server answers 409 not_revoked
// (see wantsPurge/handleRevokeKey/handleRevokeUserToken/handleRevokeOrgToken)
// when --purge targets a credential that is not yet revoked, and the CLI must
// surface that sentence and exit non-zero -- not swallow it, and not print a
// success message.
func TestRevokePurgeRefusesALiveCredential(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		run  func(ctx context.Context, c *Client, out *bytes.Buffer) error
	}{
		{
			name: "key revoke --purge",
			msg:  "revoke this key before deleting it",
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return keyRevoke(ctx, c, out,
					config.KeyRevokeCmd{Org: "acme", Project: "widgets", Key: "k1", Purge: true})
			},
		},
		{
			name: "token revoke --purge",
			msg:  "revoke this token before deleting it",
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return tokenRevoke(ctx, c, out, config.TokenRevokeCmd{Token: "t1", Purge: true})
			},
		},
		{
			name: "org robot revoke --purge",
			msg:  "revoke this token before deleting it",
			run: func(ctx context.Context, c *Client, out *bytes.Buffer) error {
				return orgRobotRevoke(ctx, c, out,
					config.OrgRobotRevokeCmd{Org: "acme", Robot: "r1", Token: "t1", Purge: true})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
				writeErr(w, http.StatusConflict, api.CodeNotRevoked, tc.msg)

				return true
			}

			c := f.client(t)

			var out bytes.Buffer

			err := tc.run(t.Context(), c, &out)
			if err == nil {
				t.Fatal("want an error, got none")
			}

			if err.Error() != tc.msg {
				t.Errorf("error = %q, want %q", err.Error(), tc.msg)
			}

			var ae *APIError
			if !errors.As(err, &ae) || ae.Code != api.CodeNotRevoked {
				t.Errorf("error = %#v, want an *APIError with code %q", err, api.CodeNotRevoked)
			}

			if out.String() != "" {
				t.Errorf("output = %q, want nothing printed on a refused purge", out.String())
			}
		})
	}
}
