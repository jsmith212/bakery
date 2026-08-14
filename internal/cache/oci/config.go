package oci

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// defaultTagTTL is how long a cached tag -> digest mapping is served without
// revalidating. Ten minutes is a deliberate middle: short enough that a `:latest`
// repointed by CI is picked up within one coffee break, long enough that a
// thousand-node cluster rolling a deployment does not put a thousand upstream HEADs
// on Docker Hub's rate limit.
//
// It is not a correctness knob. Staleness is DERIVED (now > updated_at + ttl), so
// changing it applies instantly to every already-cached tag, with no migration and no
// restart.
const defaultTagTTL = 10 * time.Minute

// defaultUpstream is the upstream a backend proxies when its config names none. Docker
// Hub, because podman and Docker Engine NEVER send ?ns= -- for them there is no other
// way to know which registry a pull is for.
const defaultUpstream = "docker.io"

// Credential is one upstream registry login.
//
// IT LIVES IN SERVER CONFIG (env / CLI), NEVER IN THE DATABASE. Three reasons, and
// the first is decisive: there is no encryption-at-rest facility in Bakery and
// pgcrypto is banned outright (migration 000001), so a DB-resident upstream credential
// is plaintext in the table, in every backup, and in every logical replication stream.
// Second, unlike a Bakery API key it cannot be hashed, because we have to REPLAY it to
// the upstream. Third, an upstream credential is an operator fact about this
// deployment, not a tenant fact about one project. Per-tenant upstream credentials are
// deferred until a tenant actually needs them, at which point they are a product +
// security decision, not an implementation detail.
type Credential struct {
	Username string
	Password string
}

// String redacts. This is not decoration: Docker Engine forwards a user's real Docker
// Hub PAT on every pull, and the upstream credentials here are the operator's. A
// Credential must be incapable of appearing in a log line, an error string or a %v,
// and the only way to make that true is to make the type itself refuse.
func (c Credential) String() string { return "oci.Credential{redacted}" }

// LogValue redacts for log/slog specifically, which does not consult String() for a
// struct value.
func (c Credential) LogValue() slog.Value { return slog.StringValue("redacted") }

// Config is the SERVER-level OCI configuration: the parts that are not per-project and
// must not be in the database.
type Config struct {
	// ExternalURL is the absolute base URL clients reach this Bakery on, e.g.
	// "https://bakery.example.com". It exists for ONE reason: the Docker Bearer
	// challenge's realm must be an ABSOLUTE URL, and a server behind a
	// TLS-terminating reverse proxy cannot derive its own scheme or host from the
	// request -- r.TLS is nil on the inside of that proxy, so a derived realm says
	// "http://" and names the internal hostname.
	//
	// That failure reproduces in NO test that talks to the server directly, and in
	// EVERY production deployment that terminates TLS at an ingress. When it is unset
	// we fall back to the request's own scheme and Host, which is correct for a direct
	// connection and for `bakery serve` on a laptop -- and wrong behind a proxy. SET
	// THIS IN ANY DEPLOYMENT WITH AN INGRESS IN FRONT OF IT.
	//
	// X-Forwarded-Proto is deliberately NOT consulted: it is a client-supplied header
	// on a route that is reachable unauthenticated, and "guess from a header an
	// attacker controls" is not an improvement over "read it from config".
	ExternalURL string

	// UpstreamAuth maps a NORMALIZED upstream host (docker.io, ghcr.io) to the
	// credential used when fetching from it. A host that is absent is fetched
	// anonymously, which is the correct default for public images.
	UpstreamAuth map[string]Credential
}

// ParseUpstreamAuth turns the CLI/env form -- "docker.io=user:pat", repeated -- into
// the map Config wants. The host is normalized, so `index.docker.io=...` and
// `docker.io=...` configure the same upstream rather than silently configuring two.
//
// The password half may itself contain ':' (a PAT with a colon is legal), so the split
// is on the FIRST ':' only. The error never contains the credential.
func ParseUpstreamAuth(pairs []string) (map[string]Credential, error) {
	out := make(map[string]Credential, len(pairs))

	for _, pair := range pairs {
		host, cred, ok := strings.Cut(pair, "=")
		if !ok || host == "" {
			return nil, fmt.Errorf("oci: upstream auth entry %d is not host=user:secret", len(out))
		}

		user, secret, ok := strings.Cut(cred, ":")
		if !ok || user == "" {
			return nil, fmt.Errorf("oci: upstream auth for %q is not user:secret", host)
		}

		out[NormalizeUpstream(host)] = Credential{Username: user, Password: secret}
	}

	return out, nil
}

// NormalizeUpstream folds the many spellings of one registry onto a single canonical
// host.
//
// It is not cosmetic. The `tags` namespace is keyed on "<host>/<name>:<tag>", so an
// unnormalized host makes ONE upstream into TWO independently-aging cache rows the
// moment two clients spell it differently -- and they do: containerd sends
// `ns=docker.io`, the registry actually answering is `registry-1.docker.io`, Docker's
// own config files say `index.docker.io`, and containerd's isProxy() has a hardcoded
// special case treating docker.io and registry-1.docker.io as the same host. Two rows
// for one tag means two TTLs, two upstream HEADs per window, and -- the part that
// bites -- two answers that can disagree about which digest `:latest` is.
//
// It also normalizes what we hand to Prometheus, which is what keeps the `upstream`
// label bounded by the operator's allowlist rather than by client spelling.
func NormalizeUpstream(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(h, "/")

	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io"
	default:
		return h
	}
}

// backendConfig is the shape the OCI backend reads out of cache_backends.config.
//
// It carries NO SECRETS -- see Credential. Everything here is ordinary operator
// configuration that is safe in a jsonb column, in a backup, and on a support ticket.
type backendConfig struct {
	// DefaultUpstream is the registry proxied when a request carries no ?ns=. podman,
	// skopeo and Docker Engine never send one, so without this they have no upstream
	// at all. Empty means docker.io.
	DefaultUpstream string `json:"default_upstream"`

	// Upstreams is the ALLOWLIST of registries this backend may dial, and it is not
	// optional -- an absent list means "the default upstream and nothing else",
	// never "anything".
	//
	// THIS IS THE SSRF GATE. ?ns= is attacker-controlled input naming a host Bakery
	// will make an outbound connection to; without an allowlist the proxy is an SSRF
	// primitive into whatever network it is deployed in, reachable by anyone who can
	// reach the cache. go-containerregistry's own SSRF hardening covers auth realms
	// and redirects -- it does NOT cover the registry host, because that is normally
	// the caller's own choice. Here it is not.
	Upstreams []string `json:"upstreams"`

	// TagTTL is the freshness horizon for a cached tag, as a Go duration string
	// ("10m", "1h"). Empty means defaultTagTTL.
	TagTTL string `json:"tag_ttl"`
}

// policy is the parsed, validated backendConfig. Parsing happens per request against
// the route's cached Config bytes, which is cheap (a few hundred bytes of JSON) and
// keeps a config edit taking effect at the route cache's TTL rather than at restart.
type policy struct {
	defaultUpstream string
	allowed         map[string]struct{}
	tagTTL          time.Duration
}

// parsePolicy reads a backend's config jsonb.
//
// IT NEVER FAILS. A malformed config blob must not take a project's registry mirror
// down -- and more importantly, it must not FAIL OPEN: the zero value of every field
// is the restrictive one (docker.io only, 10 minutes), so a typo costs an operator a
// puzzling 404 on an unlisted upstream rather than an open relay. The caller logs.
func parsePolicy(raw []byte) (policy, error) {
	var cfg backendConfig

	var err error

	if len(raw) > 0 {
		err = json.Unmarshal(raw, &cfg)
	}

	def := NormalizeUpstream(cfg.DefaultUpstream)
	if def == "" {
		def = defaultUpstream
	}

	ttl := defaultTagTTL

	if cfg.TagTTL != "" {
		d, perr := time.ParseDuration(cfg.TagTTL)
		if perr != nil || d <= 0 {
			err = fmt.Errorf("oci: bad tag_ttl %q", cfg.TagTTL)
		} else {
			ttl = d
		}
	}

	allowed := make(map[string]struct{}, len(cfg.Upstreams)+1)
	for _, u := range cfg.Upstreams {
		if n := NormalizeUpstream(u); n != "" {
			allowed[n] = struct{}{}
		}
	}

	// The default upstream is always allowed: a config that names a default it may not
	// dial is a config that can serve nothing, and silently. Listing it explicitly is
	// still fine -- the set collapses it.
	allowed[def] = struct{}{}

	return policy{defaultUpstream: def, allowed: allowed, tagTTL: ttl}, err
}

// resolveUpstream turns a request's ?ns= into the normalized host to dial, or ok=false
// when the value is not on the allowlist.
//
// Absent ?ns= is the DEFAULT upstream, not an error: podman, skopeo and Docker Engine
// never send one. A present-but-unlisted value is ok=false, which the handler renders
// as a 404 -- indistinguishable from "no such image", which is exactly right: a
// scanner probing ?ns= for internal hosts learns nothing about which ones exist.
func (p policy) resolveUpstream(ns string) (string, bool) {
	if ns == "" {
		return p.defaultUpstream, true
	}

	host := NormalizeUpstream(ns)
	if _, ok := p.allowed[host]; !ok {
		return "", false
	}

	return host, true
}
