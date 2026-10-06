package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/yohang/mesh-sdr/internal/db"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	shelldomain "github.com/yohang/mesh-sdr/internal/shell/domain"
)

type checker struct {
	origins  Origins
	problems []Problem
}

func (c *checker) fail(key, code, msg string) {
	c.problems = append(c.problems, Problem{Key: key, Origin: c.origins.Of(key).String(), Code: code, Message: msg})
}

// domainError reports a value rejected by a domain value object, with the
// domain error code when there is one.
func (c *checker) domainError(key string, err error) {
	var de *shared.Error
	if errors.As(err, &de) {
		c.fail(key, string(de.Code()), de.Message())

		return
	}

	c.fail(key, CodeInvalidValue, err.Error())
}

func (c *checker) listen(key, v string) {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		c.fail(key, CodeInvalidValue, fmt.Sprintf("invalid listen address %q: want host:port", v))

		return
	}

	if host != "" && net.ParseIP(host) == nil && !isHostname(host) {
		c.fail(key, CodeInvalidValue, fmt.Sprintf("invalid host %q", host))
	}

	if n, err := strconv.ParseUint(port, 10, 16); err != nil || port == "" || n > 65535 {
		c.fail(key, CodeInvalidValue, fmt.Sprintf("invalid port %q", port))
	}
}

func isHostname(h string) bool {
	if len(h) > 253 {
		return false
	}

	return strings.Trim(h, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-.") == ""
}

func (c *checker) enum(key, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		c.fail(key, CodeInvalidValue, fmt.Sprintf("invalid value %q: want one of %v", v, allowed))
	}
}

func (c *checker) log(l Log) {
	c.enum("log.level", l.Level, "debug", "info", "warn", "error")
	c.enum("log.format", l.Format, "text", "json")
}

func (h *Hub) validate(o Origins) []Problem {
	c := &checker{origins: o}

	c.listen("hub.listen", h.Hub.Listen)

	switch u, err := url.Parse(h.Hub.URL); {
	case h.Hub.URL == "":
		c.fail("hub.url", CodeRequired, "hub.url is required")
	case err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		c.fail("hub.url", CodeInvalidValue, fmt.Sprintf("invalid URL %q: want https://host[:port][/path]", h.Hub.URL))
	case u.Scheme != "https" && !h.Hub.AllowInsecureURL:
		c.fail("hub.url", CodeInvalidValue, "hub.url must use https (set hub.allow_insecure_url = true to allow http)")
	}

	if h.DB.DSN == "" {
		c.fail("db.dsn", CodeRequired, "db.dsn is required")
	} else if _, err := db.ParseDSN(h.DB.DSN); err != nil {
		code := CodeInvalidValue
		if errors.Is(err, db.ErrEngineUnsupported) {
			code = CodeDBEngineUnsupported
		}

		c.fail("db.dsn", code, err.Error())
	}

	if n := h.DB.MaxReadConnections; n < 1 || n > 64 {
		c.fail("db.max_read_connections", CodeInvalidValue, fmt.Sprintf("invalid value %d: want 1..64", n))
	}

	for _, id := range slices.Sorted(maps.Keys(h.Nodes)) {
		n := h.Nodes[id]
		key := "nodes." + id

		if _, err := griddomain.NewNodeID(id); err != nil {
			c.fail(key, CodeInvalidValue, "invalid node id "+strconv.Quote(id)+": must match ^[a-z0-9][a-z0-9-]{1,62}$")
		}

		if n.URL == "" {
			c.fail(key+".url", CodeRequired, key+".url is required")
		} else if _, err := griddomain.NewNodeURL(n.URL); err != nil {
			c.fail(key+".url", CodeInvalidValue, "want https://host:port")
		}

		if n.Name != "" {
			if _, err := griddomain.NewNodeName(n.Name); err != nil {
				c.fail(key+".name", CodeInvalidValue, "name must be 1 to 128 printable characters")
			}
		}

		if n.EnrollmentToken.IsSet() {
			if _, err := griddomain.ParseEnrollmentToken(n.EnrollmentToken.Reveal()); err != nil {
				c.fail(key+".enrollment_token", CodeInvalidValue, "enrollment token must be 22 to 256 printable ASCII characters")
			}
		}
	}

	if h.TLS.CACert != "" && !h.TLS.CAKey.IsSet() {
		c.fail("tls.ca_key", CodeRequired, "tls.ca_key is required with tls.ca_cert")
	}

	if h.TLS.CACert == "" && h.TLS.CAKey.IsSet() {
		c.fail("tls.ca_cert", CodeRequired, "tls.ca_cert is required with tls.ca_key")
	}

	c.log(h.Log)
	c.enum("settings.ui.theme_mode", h.Settings.UI.ThemeMode, "light", "dark", "auto")

	// Empty means "use the built-in default"; any other value must be a
	// valid policy, with the same rules as the shell's value object.
	if text := h.Settings.Receiver.UsagePolicyText; strings.TrimSpace(text) != "" {
		if _, err := shelldomain.NewPolicyText(text); err != nil {
			c.domainError("settings.receiver.usage_policy_text", err)
		}
	}

	if a := h.Auth.Argon2; a.MemoryKiB < Argon2MinMemoryKiB {
		c.fail("auth.argon2.memory_kib", CodeInvalidValue, fmt.Sprintf("invalid value %d: want >= %d", a.MemoryKiB, Argon2MinMemoryKiB))
	}

	if a := h.Auth.Argon2; a.Iterations < Argon2MinIterations {
		c.fail("auth.argon2.iterations", CodeInvalidValue, fmt.Sprintf("invalid value %d: want >= %d", a.Iterations, Argon2MinIterations))
	}

	if h.Auth.Argon2.Parallelism < 1 {
		c.fail("auth.argon2.parallelism", CodeInvalidValue, "invalid value 0: want 1..255")
	}

	c.cidrs("admin.allowed_networks", h.Admin.AllowedNetworks)
	c.cidrs("http.trusted_proxies", h.HTTP.TrustedProxies)

	return c.problems
}

// cidrs checks a list of CIDR prefixes (for example "10.0.0.0/8").
func (c *checker) cidrs(key string, v []string) {
	for _, s := range v {
		if _, err := netip.ParsePrefix(s); err != nil {
			c.fail(key, CodeInvalidValue, fmt.Sprintf("invalid CIDR %q", s))
		}
	}
}

// Prefixes parses a list of CIDR prefixes validated at load, masked and with
// IPv4-mapped IPv6 prefixes canonicalised to IPv4.
func Prefixes(v []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(v))

	for _, s := range v {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}

		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}

		out = append(out, p.Masked())
	}

	return out
}

func (n *Node) validate(o Origins) []Problem {
	c := &checker{origins: o}

	if n.Node.ID == "" {
		c.fail("node.id", CodeRequired, "node.id is required")
	} else if _, err := griddomain.NewNodeID(n.Node.ID); err != nil {
		c.domainError("node.id", err)
	}

	c.listen("node.listen", n.Node.Listen)

	if (n.TLS.Cert == "") != (n.TLS.Key == "") {
		c.fail("tls.key", CodeRequired, "tls.cert and tls.key go together")
	}

	if fp := n.HubTrust.CAFingerprint; fp != "" && !validFingerprint(fp) {
		c.fail("hub_trust.ca_fingerprint", CodeInvalidValue, "want a SHA-256 fingerprint: 64 hex digits, colons optional")
	}

	c.log(n.Log)

	return c.problems
}

func validFingerprint(s string) bool {
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 64 {
		return false
	}

	return strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}
