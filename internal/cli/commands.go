package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/config"
)

// Run dispatches one client command.
//
// main owns the server verbs (serve, migrate, version) because they need a
// database pool; everything else lands here, where the only dependency is an HTTP
// client and a token cache.
func Run(ctx context.Context, command string, cli config.CLI) error {
	out := io.Writer(os.Stdout)

	tokens, err := NewTokenStore()
	if err != nil {
		return err
	}

	client, err := NewClient(cli.Server, tokens)
	if err != nil {
		return err
	}

	r := renderer{out: out, json: cli.JSON}

	switch command {
	case "login":
		return Login(ctx, client, out)

	case "logout":
		return Logout(client, out)

	case "whoami":
		return whoami(ctx, client, r)

	case "org list":
		return orgList(ctx, client, r)
	case "org create":
		return orgCreate(ctx, client, r, cli.Org.Create)
	case "org show":
		return orgShow(ctx, client, r, cli.Org.Show.Org)
	case "org rename":
		return orgRename(ctx, client, r, cli.Org.Rename)
	case "org delete":
		return orgDelete(ctx, client, out, cli.Org.Delete)

	case "project list":
		return projectList(ctx, client, r, cli.Project.List.Org)
	case "project create":
		return projectCreate(ctx, client, r, cli.Project.Create)
	case "project show":
		return projectShow(ctx, client, r, cli.Project.Show)
	case "project rename":
		return projectRename(ctx, client, r, cli.Project.Rename)
	case "project delete":
		return projectDelete(ctx, client, out, cli.Project.Delete)

	case "member list":
		return memberList(ctx, client, r, cli.Member.List)
	case "member set":
		return memberSet(ctx, client, r, cli.Member.Set)
	case "member remove":
		return memberRemove(ctx, client, out, cli.Member.Remove)

	case "key list":
		return keyList(ctx, client, r, cli.Key.List)
	case "key create":
		return keyCreate(ctx, client, r, out, cli.Key.Create)
	case "key revoke":
		return keyRevoke(ctx, client, out, cli.Key.Revoke)

	default:
		return fmt.Errorf("unknown command: %q", command)
	}
}

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

func whoami(ctx context.Context, c *Client, r renderer) error {
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}

	return r.value(me, func(out io.Writer) {
		pairs := [][2]string{
			{"email", me.Email},
			{"name", dash(me.DisplayName)},
			{"method", me.Method},
			{"site role", me.SiteRole},
		}

		for i, o := range me.Orgs {
			key := ""
			if i == 0 {
				key = "orgs"
			}

			pairs = append(pairs, [2]string{key, fmt.Sprintf("%s (%s)", o.Slug, o.Role)})
		}

		for i, p := range me.Projects {
			key := ""
			if i == 0 {
				key = "projects"
			}

			pairs = append(pairs, [2]string{key,
				fmt.Sprintf("%s/%s (%s)", p.OrgSlug, p.Slug, p.Role)})
		}

		fields(out, pairs)
	})
}

// ---------------------------------------------------------------------------
// org
// ---------------------------------------------------------------------------

func orgList(ctx context.Context, c *Client, r renderer) error {
	orgs, err := c.ListOrgs(ctx)
	if err != nil {
		return err
	}

	rows := make([][]string, 0, len(orgs))
	for _, o := range orgs {
		rows = append(rows, []string{o.Slug, o.Name, dash(o.Role)})
	}

	return r.list(orgs, []string{"slug", "name", "your role"}, rows, "no organizations")
}

func orgCreate(ctx context.Context, c *Client, r renderer, cmd config.OrgCreateCmd) error {
	name := cmd.Name
	if name == "" {
		name = cmd.Slug
	}

	org, err := c.CreateOrg(ctx, cmd.Slug, name)
	if err != nil {
		return err
	}

	return r.value(org, func(out io.Writer) {
		fmt.Fprintf(out, "created organization %s\n", org.Slug)
	})
}

func orgShow(ctx context.Context, c *Client, r renderer, slug string) error {
	org, err := c.GetOrg(ctx, slug)
	if err != nil {
		return err
	}

	return r.value(org, func(out io.Writer) {
		fields(out, [][2]string{
			{"slug", org.Slug},
			{"name", org.Name},
			{"id", org.ID},
			{"your role", dash(org.Role)},
			{"created", org.CreatedAt.UTC().Format(time.RFC3339)},
		})
	})
}

func orgRename(ctx context.Context, c *Client, r renderer, cmd config.OrgRenameCmd) error {
	org, err := c.RenameOrg(ctx, cmd.Org, cmd.Name)
	if err != nil {
		return err
	}

	return r.value(org, func(out io.Writer) {
		fmt.Fprintf(out, "renamed %s to %q\n", org.Slug, org.Name)
	})
}

func orgDelete(ctx context.Context, c *Client, out io.Writer, cmd config.OrgDeleteCmd) error {
	// The slug is the first path segment of every cache URL under this org, and the
	// delete cascades to every project, key and cached object beneath it. There is
	// nobody to answer an interactive prompt in a CI job, so the confirmation is a
	// flag.
	if !cmd.Yes {
		return fmt.Errorf(
			"deleting %s removes its projects, keys and cached objects; pass --yes to confirm", cmd.Org)
	}

	if err := c.DeleteOrg(ctx, cmd.Org); err != nil {
		return err
	}

	fmt.Fprintf(out, "deleted organization %s\n", cmd.Org)

	return nil
}

// ---------------------------------------------------------------------------
// project
// ---------------------------------------------------------------------------

func projectList(ctx context.Context, c *Client, r renderer, org string) error {
	projects, err := c.ListProjects(ctx, org)
	if err != nil {
		return err
	}

	rows := make([][]string, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, []string{
			p.Slug, p.Name, dash(p.Role), dash(strings.Join(p.Backends, ",")),
		})
	}

	return r.list(projects,
		[]string{"slug", "name", "your role", "backends"}, rows,
		"no projects in "+org)
}

func projectCreate(ctx context.Context, c *Client, r renderer, cmd config.ProjectCreateCmd) error {
	name := cmd.Name
	if name == "" {
		name = cmd.Slug
	}

	p, err := c.CreateProject(ctx, cmd.Org, cmd.Slug, name)
	if err != nil {
		return err
	}

	return r.value(p, func(out io.Writer) {
		fmt.Fprintf(out, "created project %s/%s\n", p.OrgSlug, p.Slug)
	})
}

func projectShow(ctx context.Context, c *Client, r renderer, cmd config.ProjectShowCmd) error {
	p, err := c.GetProject(ctx, cmd.Org, cmd.Project)
	if err != nil {
		return err
	}

	return r.value(p, func(out io.Writer) {
		fields(out, [][2]string{
			{"slug", p.OrgSlug + "/" + p.Slug},
			{"name", p.Name},
			{"id", p.ID},
			{"your role", dash(p.Role)},
			{"backends", dash(strings.Join(p.Backends, ", "))},
			{"created", p.CreatedAt.UTC().Format(time.RFC3339)},
		})
	})
}

func projectRename(ctx context.Context, c *Client, r renderer, cmd config.ProjectRenameCmd) error {
	p, err := c.RenameProject(ctx, cmd.Org, cmd.Project, cmd.Name)
	if err != nil {
		return err
	}

	return r.value(p, func(out io.Writer) {
		fmt.Fprintf(out, "renamed %s/%s to %q\n", p.OrgSlug, p.Slug, p.Name)
	})
}

func projectDelete(ctx context.Context, c *Client, out io.Writer, cmd config.ProjectDeleteCmd) error {
	if !cmd.Yes {
		return fmt.Errorf(
			"deleting %s/%s removes its keys and cached objects; pass --yes to confirm",
			cmd.Org, cmd.Project)
	}

	if err := c.DeleteProject(ctx, cmd.Org, cmd.Project); err != nil {
		return err
	}

	fmt.Fprintf(out, "deleted project %s/%s\n", cmd.Org, cmd.Project)

	return nil
}

// ---------------------------------------------------------------------------
// member
// ---------------------------------------------------------------------------

func memberList(ctx context.Context, c *Client, r renderer, cmd config.MemberListCmd) error {
	// With no project, list the org's roster and its claim-derived org roles. With
	// one, list the same people plus their project role -- including the members
	// who have none, because those are exactly the people you are about to grant
	// one to.
	if cmd.Project == "" {
		members, err := c.ListOrgMembers(ctx, cmd.Org)
		if err != nil {
			return err
		}

		rows := make([][]string, 0, len(members))
		for _, m := range members {
			rows = append(rows, []string{m.Email, dash(m.DisplayName), m.OrgRole, m.Source})
		}

		return r.list(members,
			[]string{"email", "name", "org role", "source"}, rows,
			"no members in "+cmd.Org)
	}

	members, err := c.ListProjectMembers(ctx, cmd.Org, cmd.Project)
	if err != nil {
		return err
	}

	rows := make([][]string, 0, len(members))
	for _, m := range members {
		rows = append(rows, []string{
			m.Email, dash(m.DisplayName), m.OrgRole, dash(m.ProjectRole),
		})
	}

	return r.list(members,
		[]string{"email", "name", "org role", "project role"}, rows,
		"no members in "+cmd.Org+"/"+cmd.Project)
}

func memberSet(ctx context.Context, c *Client, r renderer, cmd config.MemberSetCmd) error {
	m, err := c.SetProjectMember(ctx, cmd.Org, cmd.Project, cmd.User, cmd.Role)
	if err != nil {
		return err
	}

	return r.value(m, func(out io.Writer) {
		fmt.Fprintf(out, "%s is now %s on %s/%s\n",
			m.Email, m.ProjectRole, cmd.Org, cmd.Project)
	})
}

func memberRemove(ctx context.Context, c *Client, out io.Writer, cmd config.MemberRemoveCmd) error {
	if err := c.RemoveProjectMember(ctx, cmd.Org, cmd.Project, cmd.User); err != nil {
		return err
	}

	fmt.Fprintf(out, "removed %s from %s/%s\n", cmd.User, cmd.Org, cmd.Project)

	return nil
}

// ---------------------------------------------------------------------------
// key
// ---------------------------------------------------------------------------

func keyList(ctx context.Context, c *Client, r renderer, cmd config.KeyListCmd) error {
	keys, err := c.ListKeys(ctx, cmd.Org, cmd.Project)
	if err != nil {
		return err
	}

	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{
			k.ID, k.Name, k.TokenPrefix, k.Scope, k.OwnerEmail,
			ts(k.ExpiresAt), ts(k.LastUsedAt), ts(k.RevokedAt),
		})
	}

	return r.list(keys,
		[]string{"id", "name", "prefix", "scope", "owner", "expires", "last used", "revoked"},
		rows, "no keys in "+cmd.Org+"/"+cmd.Project)
}

func keyCreate(
	ctx context.Context, c *Client, r renderer, out io.Writer, cmd config.KeyCreateCmd,
) error {
	var expiresAt *time.Time

	if cmd.ExpiresIn > 0 {
		t := time.Now().Add(cmd.ExpiresIn).UTC()
		expiresAt = &t
	}

	key, err := c.CreateKey(ctx, cmd.Org, cmd.Project, cmd.Name, cmd.Scope, expiresAt)
	if err != nil {
		return err
	}

	// --json emits the CreatedAPIKey verbatim, token and all, because a script that
	// mints a key needs to capture it and there is no second chance to fetch it.
	return r.value(key, func(io.Writer) {
		printCreatedKey(out, key)
	})
}

// printCreatedKey is the one-time reveal.
//
// The server stores only the SHA-256 of this token. There is no query, no admin
// path and no database dump that can recover it -- so if this scrollback is lost,
// the key is lost, and the remedy is to mint another one. Say that plainly, on
// its own line, rather than trusting the user to infer it.
func printCreatedKey(out io.Writer, key api.CreatedAPIKey) {
	fmt.Fprintf(out, "\ncreated key %q on %s\n\n", key.Name, key.ProjectID)
	fmt.Fprintf(out, "  %s\n\n", key.Token)
	fmt.Fprint(out, "this is the only time the token is shown. it is stored as a hash and\n")
	fmt.Fprint(out, "cannot be recovered -- copy it now, or mint a new key.\n\n")

	fields(out, [][2]string{
		{"  id", key.ID},
		{"  scope", key.Scope},
		{"  expires", ts(key.ExpiresAt)},
	})
}

func keyRevoke(ctx context.Context, c *Client, out io.Writer, cmd config.KeyRevokeCmd) error {
	if err := c.DeleteKey(ctx, cmd.Org, cmd.Project, cmd.Key); err != nil {
		return err
	}

	fmt.Fprintf(out, "revoked key %s\n", cmd.Key)

	return nil
}
