package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
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

	u, err := url.Parse(h.Hub.URL)
	c.gateway(h, u, err)

	switch {
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
				c.fail(key+".enrollment_token", CodeInvalidValue, "enrollment token must be 32 random bytes in unpadded base64url (43 characters), for example `openssl rand -base64 32 | tr +/ -_ | tr -d =`")
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
	c.settings(h)

	if a := h.Auth.Argon2; a.MemoryKiB < Argon2MinMemoryKiB {
		c.fail("auth.argon2.memory_kib", CodeInvalidValue, fmt.Sprintf("invalid value %d: want >= %d", a.MemoryKiB, Argon2MinMemoryKiB))
	}

	if a := h.Auth.Argon2; a.Iterations < Argon2MinIterations {
		c.fail("auth.argon2.iterations", CodeInvalidValue, fmt.Sprintf("invalid value %d: want >= %d", a.Iterations, Argon2MinIterations))
	}

	if h.Auth.Argon2.Parallelism < 1 {
		c.fail("auth.argon2.parallelism", CodeInvalidValue, "invalid value 0: want 1..255")
	}

	if h.Auth.TokenKeyDir == "" {
		c.fail("auth.token_key_dir", CodeRequired, "auth.token_key_dir is required")
	}

	if d := h.Auth.TokenTTL.Duration(); d < time.Minute || d > 10*time.Minute {
		c.fail("auth.token_ttl", CodeInvalidValue, fmt.Sprintf("invalid value %s: want 1m to 10m (SR-44)", h.Auth.TokenTTL))
	}

	if n := h.Auth.KeyRotationDays; n < 1 || n > 365 {
		c.fail("auth.key_rotation_days", CodeInvalidValue, fmt.Sprintf("invalid value %d: want 1..365", n))
	}

	c.cidrs("admin.allowed_networks", h.Admin.AllowedNetworks)
	c.cidrs("http.trusted_proxies", h.HTTP.TrustedProxies)
	c.smtp(h.SMTP)

	return c.problems
}

// smtp checks the mail relay settings when mail is configured (SR-09: TLS
// with a verified certificate unless explicitly allowed off).
func (c *checker) smtp(s SMTP) {
	c.enum("smtp.tls", s.TLS, "starttls", "implicit", "none")

	if !s.Enabled() {
		return
	}

	if net.ParseIP(s.Host) == nil && !isHostname(s.Host) {
		c.fail("smtp.host", CodeInvalidValue, fmt.Sprintf("invalid host %q", s.Host))
	}

	if s.Port < 1 || s.Port > 65535 {
		c.fail("smtp.port", CodeInvalidValue, fmt.Sprintf("invalid value %d: want 1..65535", s.Port))
	}

	if s.TLS == "none" && !s.AllowInsecure {
		c.fail("smtp.tls", CodeInvalidValue, "smtp.tls = none sends mail in clear: set smtp.allow_insecure = true to allow it")
	}

	switch a, err := mail.ParseAddress(s.From); {
	case s.From == "":
		c.fail("smtp.from", CodeRequired, "smtp.from is required with smtp.host")
	case err != nil || a.Address == "":
		c.fail("smtp.from", CodeInvalidValue, fmt.Sprintf("invalid address %q", s.From))
	}
}

// gateway checks the [gateway] table.
func (c *checker) gateway(h *Hub, hubURL *url.URL, hubURLErr error) {
	g := h.Gateway

	c.enum("gateway.tls_mode", g.TLSMode, TLSModeACME, TLSModeFiles, TLSModeOff)

	if g.TLSMode == TLSModeOff {
		if g.HTTPListen == "" {
			c.fail("gateway.http_listen", CodeRequired, "gateway.http_listen is required with gateway.tls_mode = off")
		}
	} else {
		if g.HTTPSListen == "" {
			c.fail("gateway.https_listen", CodeRequired, "gateway.https_listen is required unless gateway.tls_mode = off")
		} else {
			c.listen("gateway.https_listen", g.HTTPSListen)
		}
	}

	if g.HTTPListen != "" {
		c.listen("gateway.http_listen", g.HTTPListen)
	}

	if g.HTTPListen != "" && g.HTTPListen == g.HTTPSListen && g.TLSMode != TLSModeOff {
		c.fail("gateway.http_listen", CodeInvalidValue, "gateway.http_listen and gateway.https_listen must differ")
	}

	if g.TLSMode == TLSModeFiles {
		if g.TLSCert == "" {
			c.fail("gateway.tls_cert", CodeRequired, "gateway.tls_cert is required with gateway.tls_mode = files")
		}

		if g.TLSKey == "" {
			c.fail("gateway.tls_key", CodeRequired, "gateway.tls_key is required with gateway.tls_mode = files")
		} else {
			c.privateKeyFile("gateway.tls_key", g.TLSKey)
		}
	}

	if g.TLSMode == TLSModeACME {
		if hubURLErr == nil && hubURL.Hostname() != "" {
			if host := hubURL.Hostname(); net.ParseIP(host) != nil || !strings.Contains(host, ".") {
				c.fail("gateway.tls_mode", CodeInvalidValue, fmt.Sprintf("ACME needs a public DNS name in hub.url, not %q: use files or internal", host))
			}
		}
	}

	if g.ACMECA != "" {
		if u, err := url.Parse(g.ACMECA); err != nil || u.Scheme != "https" || u.Host == "" {
			c.fail("gateway.acme_ca", CodeInvalidValue, "want the https URL of an ACME directory")
		}
	}

	if g.TLSMode == TLSModeACME && g.StorageDir == "" {
		c.fail("gateway.storage_dir", CodeRequired, "gateway.storage_dir is required with gateway.tls_mode = acme")
	}

	if g.StreamTimeout.Duration() <= 0 {
		c.fail("gateway.stream_timeout", CodeInvalidValue, "want a positive duration")
	}

	if g.MaxBody.Bytes() < 64<<10 {
		c.fail("gateway.max_body", CodeInvalidValue, "want at least 64KiB")
	}
}

// privateKeyFile checks that a private key file exists and is readable by
// its owner only (mode 0600 or stricter).
func (c *checker) privateKeyFile(key, path string) {
	info, err := os.Stat(path)
	if err != nil {
		c.fail(key, CodeInvalidValue, fmt.Sprintf("cannot read %s: %v", path, err))

		return
	}

	if info.Mode().Perm()&0o077 != 0 {
		c.fail(key, CodeInsecureSecretFile, fmt.Sprintf("%s has mode %04o, want 0600", path, info.Mode().Perm()))
	}
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

	if v := n.Node.EventBuffer.MaxEvents; v < 100 || v > 10_000_000 {
		c.fail("node.event_buffer.max_events", CodeInvalidValue, fmt.Sprintf("invalid value %d: want 100..10000000", v))
	}

	if v := n.Node.EventBuffer.MaxBytes.Bytes(); v < 64<<10 || v > 4<<30 {
		c.fail("node.event_buffer.max_bytes", CodeInvalidValue, "want 64KiB..4GiB")
	}

	if d := n.Node.RuntimeDir; !filepath.IsAbs(d) || filepath.Clean(d) != d {
		c.fail("node.runtime_dir", CodeInvalidValue, "want an absolute, clean path")
	}

	if _, _, err := ParsePortRange(n.Node.IPCPortRange); err != nil {
		c.fail("node.ipc_port_range", CodeInvalidValue, err.Error())
	}

	if v := n.Node.MaxDemods; v < 1 || v > 1000 {
		c.fail("node.max_demods", CodeInvalidValue, "want 1..1000")
	}

	if v := n.Node.WSNotSentLowat.Bytes(); v < 1<<10 || v > 1<<20 {
		c.fail("node.ws_notsent_lowat", CodeInvalidValue, "want 1KiB..1MiB")
	}

	for i, d := range n.Tools.Dirs {
		if !filepath.IsAbs(d) || strings.Contains(d, ":") {
			c.fail("tools.dirs["+strconv.Itoa(i)+"]", CodeInvalidValue, "want an absolute path without ':'")
		}
	}

	for _, t := range n.Tools.all() {
		if t.path != "" && !filepath.IsAbs(t.path) {
			c.fail("tools."+t.key, CodeInvalidValue, "want an absolute path")
		}
	}

	if v := n.Decoders.BatchWorkers; v < 0 || v > 256 {
		c.fail("decoders.batch_workers", CodeInvalidValue, "want 0..256")
	}

	if v := n.Decoders.MaxSessions; v < 0 || v > 10000 {
		c.fail("decoders.max_sessions", CodeInvalidValue, "want 0..10000")
	}

	if v := n.Decoders.QueueLength; v < 1 || v > 1000 {
		c.fail("decoders.queue_length", CodeInvalidValue, "want 1..1000")
	}

	if v := n.Decoders.ProcessLimits.Nice; v < 0 || v > 19 {
		c.fail("decoders.process_limits.nice", CodeInvalidValue, "want 0..19")
	}

	if v := n.Decoders.ProcessLimits.OpenFiles; v < 16 || v > 1<<20 {
		c.fail("decoders.process_limits.open_files", CodeInvalidValue, "want 16..1048576")
	}

	if v := n.Decoders.ProcessLimits.Memory.Bytes(); v != 0 && v < 64<<20 {
		c.fail("decoders.process_limits.memory", CodeInvalidValue, "want 0 (none) or at least 64MiB")
	}

	for _, id := range slices.Sorted(maps.Keys(n.Devices)) {
		d := n.Devices[id]
		key := "devices." + id

		if _, err := shared.NewDeviceID(id); err != nil {
			c.fail(key, CodeInvalidValue, "invalid device id "+strconv.Quote(id)+": must match "+shared.DeviceIDPattern)
		}

		if d.Name == "" || len(d.Name) > 128 {
			c.fail(key+".name", CodeRequired, "name is required (1 to 128 characters)")
		}

		if d.Type == "" || len(d.Type) > 48 {
			c.fail(key+".type", CodeRequired, "type is required (1 to 48 characters)")
		}

		if lo, hi := d.FreqRange.Min.Hz(), d.FreqRange.Max.Hz(); lo <= 0 || hi <= lo {
			c.fail(key+".freq_range", CodeInvalidValue, "want 0 < min < max")
		}

		if len(d.SampleRates) == 0 || slices.ContainsFunc(d.SampleRates, func(r int64) bool { return r <= 0 }) {
			c.fail(key+".sample_rates", CodeInvalidValue, "want at least one positive sample rate")
		}

		if d.ListenPolicy != "" {
			c.enum(key+".listen_policy", d.ListenPolicy, griddomain.ListenAnonymous, griddomain.ListenRegistered)
		}

		if d.MaxDemods < 0 || d.MaxDemods > 1000 {
			c.fail(key+".max_demods", CodeInvalidValue, "want 0..1000 (0: default 16)")
		}

	}

	if (n.TLS.Cert == "") != (n.TLS.Key == "") {
		c.fail("tls.key", CodeRequired, "tls.cert and tls.key go together")
	}

	if fp := n.HubTrust.CAFingerprint; fp != "" && !validFingerprint(fp) {
		c.fail("hub_trust.ca_fingerprint", CodeInvalidValue, "want a SHA-256 fingerprint: 64 hex digits, colons optional")
	}

	c.log(n.Log)

	return c.problems
}

// settings validates the [settings] keys set in a file or the env with the
// settings validator (ADR 0010), then the checks across keys when every key
// of a check is set in the config (combinations with DB values are checked
// by the settings store).
func (c *checker) settings(h *Hub) {
	idx, err := loadSettingsIndex()
	if err != nil {
		c.fail("settings", CodeInvalidValue, err.Error())

		return
	}

	values := map[string]any{}

	for _, lf := range leaves(h) {
		key, ok := strings.CutPrefix(lf.key, settingsPrefix)
		if !ok || !c.origins.Of(lf.key).Locked() {
			continue
		}

		v := lf.value.Interface()

		raw, err := json.Marshal(v)
		if err != nil {
			c.fail(lf.key, CodeInvalidValue, err.Error())

			continue
		}

		for _, vi := range checkSetting(idx.byKey[key], v, raw) {
			c.fail(settingsPrefix+vi.Path(), string(vi.Code()), vi.Message())
		}

		values[key] = v
	}

	for _, vi := range CheckSettings(func(k string) (any, bool) { v, ok := values[k]; return v, ok }) {
		c.fail(settingsPrefix+vi.Path(), string(vi.Code()), vi.Message())
	}
}

// validFingerprint reports whether s is a SHA-256 fingerprint: 64 hex
// digits, colons optional.
func validFingerprint(s string) bool {
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 64 {
		return false
	}

	return strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}
