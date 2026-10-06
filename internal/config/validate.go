package config

import (
	"errors"
	"fmt"
	"net"
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

	c.log(h.Log)
	c.enum("settings.ui.theme_mode", h.Settings.UI.ThemeMode, "light", "dark", "auto")

	// Empty means "use the built-in default"; any other value must be a
	// valid policy, with the same rules as the shell's value object.
	if text := h.Settings.Receiver.UsagePolicyText; strings.TrimSpace(text) != "" {
		if _, err := shelldomain.NewPolicyText(text); err != nil {
			c.domainError("settings.receiver.usage_policy_text", err)
		}
	}

	return c.problems
}

func (n *Node) validate(o Origins) []Problem {
	c := &checker{origins: o}

	if n.Node.ID == "" {
		c.fail("node.id", CodeRequired, "node.id is required")
	} else if _, err := griddomain.NewNodeID(n.Node.ID); err != nil {
		c.domainError("node.id", err)
	}

	c.listen("node.listen", n.Node.Listen)
	c.log(n.Log)

	return c.problems
}
