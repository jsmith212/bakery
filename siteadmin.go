package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The BREAK-GLASS. `bakery user site-admin <email>`.
//
// # The deadlock it exists to break
//
// With `login_groups` empty and no `site_admin_groups`, a fresh deployment has NO
// site admin -- and every path to making one requires already being one. There is no
// API endpoint that can fix that, and there must not be: an endpoint that could mint
// the first site admin without already having one is an unauthenticated
// privilege-escalation endpoint however it is dressed, and gating it on "only while
// there are no site admins yet" is a race with whoever finds it first.
//
// So this path IS NOT ON THE NETWORK. It lives in main, not in internal/cli -- which
// is the HTTP client and has no database dependency at all -- and it holds a DSN, not
// a session. Reaching it requires infrastructure access. That is the same shape as
// DEV_LOGIN_ENABLED, which is settable only by env var or flag and which no UI or API
// path can turn on.
//
// It is not a privilege escalation for anyone who can already run it: whoever holds
// DB_URL could `UPDATE users SET site_role_local = 'admin'` by hand. What this buys
// is that they do not have to, and that they cannot get the columns subtly wrong --
// writing site_role_OIDC by hand would forge a claim that the user's next login would
// rightly reconcile away, leaving an admin who was one until they signed in again.
//
// # It is deliberately NOT gated by --allow-local-site-admins
//
// That flag turns off the API path. Gating this too would buy no security (see
// above) and would make a locked-down deployment with no site admin permanently
// unbootstrappable -- which is the exact deadlock this command exists for.
//
// Every grant it makes is VISIBLE: it lands in the site-admin listing as
// `site_role_source: local` with no granter named, because there is no session and so
// nobody to name. That emptiness is a finding, not a gap. It says: someone with
// database access made this.
func userSiteAdmin(ctx context.Context, out io.Writer, cmd config.UserSiteAdminCmd) error {
	// The serving pool, not the bootstrap one: the schema must already exist. If it
	// does not, the enum registration refuses the connection and says so, which is a
	// better error than a missing-column failure three statements later.
	pool, err := db.NewPool(ctx, db.Config{URL: cmd.DBURL, MaxConns: 0})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	defer pool.Close()

	store := db.NewStore(pool)

	user, err := store.GetUserByEmail(ctx, cmd.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Users are JIT-provisioned at their first login, so there is genuinely
			// nothing to grant yet. Say that, rather than "not found" -- the fix is for
			// the person to sign in once, and nobody guesses that from a 404.
			return fmt.Errorf("no user with email %q. Users are provisioned at their first "+
				"login: ask them to sign in once, then run this again", cmd.Email)
		}

		return fmt.Errorf("look up user %q: %w", cmd.Email, err)
	}

	if cmd.Revoke {
		return revokeSiteAdmin(ctx, out, store, user)
	}

	// site_role_LOCAL, with provenance, and never site_role_oidc. The local half is
	// the half the login reconciler does not name, so this grant survives every login
	// the user ever makes. granted_by is left NULL: there is no session here, so there
	// is nobody to name, and pretending otherwise would put a lie in the audit trail.
	granted, err := store.GrantSiteAdminLocal(ctx, repository.GrantSiteAdminLocalParams{
		ID:        user.ID,
		GrantedBy: pgtype.UUID{}, //nolint:exhaustruct // the zero UUID is SQL NULL: no session, no granter.
	})
	if err != nil {
		return fmt.Errorf("grant the local site-admin role to %q: %w", cmd.Email, err)
	}

	fmt.Fprintf(out, "%s is now a site administrator (local grant, no granter recorded).\n", granted.Email)
	fmt.Fprintln(out, "It is visible as `local` in the site-admin listing, and it SURVIVES their "+
		"next login -- revoke it with --revoke, or in the console.")

	return nil
}

// revokeSiteAdmin clears the LOCAL half. It cannot touch a claim-derived site role,
// and it says so instead of reporting a success that changed nothing: an operator who
// believes they have demoted a site admin who is still one has a security incident,
// not a typo.
func revokeSiteAdmin(ctx context.Context, out io.Writer, store *db.Store, user repository.User) error {
	if !user.SiteRoleLocal.Valid {
		if user.SiteRoleOidc.Valid {
			return fmt.Errorf("%s is a site administrator by OIDC GROUP CLAIM%s, not by a local "+
				"grant, so there is nothing here to revoke. Remove them from the group in the "+
				"identity provider, or from site_admin_groups, and the role disappears at their "+
				"next login", user.Email, claimGroup(user))
		}

		return fmt.Errorf("%s holds no local site-admin grant", user.Email)
	}

	revoked, err := store.RevokeSiteAdminLocal(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("revoke the local site-admin role from %q: %w", user.Email, err)
	}

	// The effective role is greatest(oidc, local), computed by the database. Read it
	// back rather than assuming: if an OIDC group ALSO makes them an admin, clearing
	// the local half demoted nobody, and the operator has to hear that.
	if revoked.SiteRole == repository.SiteRoleAdmin {
		fmt.Fprintf(out, "The local grant was removed, but %s IS STILL A SITE ADMINISTRATOR: "+
			"their OIDC group claims%s make them one on their own.\n", revoked.Email, claimGroup(revoked))
		fmt.Fprintln(out, "To remove the role entirely, remove them from the group in the identity provider.")

		return nil
	}

	fmt.Fprintf(out, "%s is no longer a site administrator.\n", revoked.Email)

	return nil
}

// claimGroup names the group holding a claim-derived site role up, when we know it.
func claimGroup(u repository.User) string {
	if !u.SiteOidcGroup.Valid || u.SiteOidcGroup.String == "" {
		return ""
	}

	return " (" + u.SiteOidcGroup.String + ")"
}
