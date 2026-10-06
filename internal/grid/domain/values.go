package domain

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// NodeName is the display name of a node: 1 to 128 printable characters,
// plain text.
type NodeName struct{ value string }

// NewNodeName validates s.
func NewNodeName(s string) (NodeName, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 128 || !utf8.ValidString(s) {
		return NodeName{}, ErrInvalidNodeName
	}

	for _, r := range s {
		if unicode.IsControl(r) {
			return NodeName{}, ErrInvalidNodeName
		}
	}

	return NodeName{value: s}, nil
}

// MustNodeName is NewNodeName that panics. Tests and constants only.
func MustNodeName(s string) NodeName {
	n, err := NewNodeName(s)
	if err != nil {
		panic(err)
	}

	return n
}

// String returns the name.
func (n NodeName) String() string { return n.value }

// NodeURL is the base URL the hub dials: https://host:port, no path.
type NodeURL struct{ value string }

// NewNodeURL validates s.
func NewNodeURL(s string) (NodeURL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return NodeURL{}, ErrInvalidNodeURL.WithDetail("invalid node url " + strconv.Quote(s) + ": want https://host:port")
	}

	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" {
		return NodeURL{}, ErrInvalidNodeURL.WithDetail("invalid node url " + strconv.Quote(s) + ": the port is required")
	}

	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return NodeURL{}, ErrInvalidNodeURL.WithDetail("invalid node url " + strconv.Quote(s) + ": invalid port")
	}

	return NodeURL{value: "https://" + u.Host}, nil
}

// MustNodeURL is NewNodeURL that panics. Tests and constants only.
func MustNodeURL(s string) NodeURL {
	u, err := NewNodeURL(s)
	if err != nil {
		panic(err)
	}

	return u
}

// String returns the URL.
func (u NodeURL) String() string { return u.value }

// Host returns the host of the URL (an IP or a DNS name, no port).
func (u NodeURL) Host() string {
	parsed, err := url.Parse(u.value)
	if err != nil {
		return ""
	}

	return parsed.Hostname()
}

// Endpoint returns the URL of path on the node with scheme (https or wss).
func (u NodeURL) Endpoint(scheme, path string) string {
	return scheme + strings.TrimPrefix(u.value, "https") + path
}

// EnrollmentToken is the secret shared by the hub and an enrolling node:
// 32 random bytes (256 bits) in unpadded base64url, 43 characters. Config
// tokens must have the same format, so that the HMAC proofs the hub sends
// before the node is authenticated cannot be brute-forced offline
// (ADR 0008).
type EnrollmentToken struct{ value string }

// NewEnrollmentToken returns a fresh random token.
func NewEnrollmentToken() (EnrollmentToken, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return EnrollmentToken{}, fmt.Errorf("generate enrollment token: %w", err)
	}

	return EnrollmentToken{value: base64.RawURLEncoding.EncodeToString(b[:])}, nil
}

// ParseEnrollmentToken validates s: 32 bytes in unpadded base64url.
func ParseEnrollmentToken(s string) (EnrollmentToken, error) {
	s = strings.TrimSpace(s)

	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != 32 {
		return EnrollmentToken{}, ErrInvalidEnrollmentToken
	}

	return EnrollmentToken{value: s}, nil
}

// String returns the token. Show it once, never log it.
func (t EnrollmentToken) String() string { return t.value }

// Key derives the HMAC key of the enrollment exchange (ADR 0008):
// HKDF-SHA256(token, info "meshsdr enroll v1").
func (t EnrollmentToken) Key() EnrollmentKey {
	k, err := hkdf.Key(sha256.New, []byte(t.value), nil, "meshsdr enroll v1", 32)
	if err != nil {
		panic(err) // unreachable: 32 bytes is far below the HKDF limit
	}

	var key EnrollmentKey
	copy(key.k[:], k)

	return key
}

// EnrollmentKey is the HMAC key derived from an enrollment token. The hub
// stores it instead of the token.
type EnrollmentKey struct{ k [32]byte }

// EnrollmentKeyFromBytes rehydrates a stored key.
func EnrollmentKeyFromBytes(b []byte) (EnrollmentKey, error) {
	var k EnrollmentKey
	if len(b) != len(k.k) {
		return k, ErrInvalidEnrollmentToken.WithDetail("stored enrollment key must be 32 bytes")
	}

	copy(k.k[:], b)

	return k, nil
}

// Bytes returns a copy of the key.
func (k EnrollmentKey) Bytes() []byte { return append([]byte(nil), k.k[:]...) }

// EnrollmentNonceSize is the size of the nonce of an enrollment exchange.
const EnrollmentNonceSize = 32

// Domain-separation labels of the three enrollment MACs.
const (
	labelHello = "meshsdr enroll v1 hello"
	labelCSR   = "meshsdr enroll v1 csr"
	labelChain = "meshsdr enroll v1 chain"
)

// mac is HMAC(K, label ‖ 0 ‖ len-prefixed parts): each MAC is bound to its
// phase and its fields cannot be shifted across boundaries.
func (k EnrollmentKey) mac(label string, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, k.k[:])
	m.Write([]byte(label))
	m.Write([]byte{0})

	for _, p := range parts {
		var n [4]byte

		binary.BigEndian.PutUint32(n[:], uint32(len(p))) //nolint:gosec // parts are far below 4 GiB
		m.Write(n[:])
		m.Write(p)
	}

	return m.Sum(nil)
}

// HelloProof is the hub proof of phase 1, over the nonce, the node id and
// the node self-signed certificate hash.
func (k EnrollmentKey) HelloProof(nonce []byte, id NodeID, selfSignedHash [32]byte) []byte {
	return k.mac(labelHello, nonce, []byte(id.String()), selfSignedHash[:])
}

// CSRMAC is the node answer of phase 1, over the nonce and the CSR hash.
func (k EnrollmentKey) CSRMAC(nonce []byte, csrDER []byte) []byte {
	h := sha256.Sum256(csrDER)

	return k.mac(labelCSR, nonce, h[:])
}

// ChainMAC authenticates the certificate chain of phase 2, over the nonce
// and the hash of each certificate.
func (k EnrollmentKey) ChainMAC(nonce []byte, chain [][]byte) []byte {
	parts := [][]byte{nonce}

	for _, c := range chain {
		h := sha256.Sum256(c)
		parts = append(parts, h[:])
	}

	return k.mac(labelChain, parts...)
}

// VerifyMAC compares two MACs in constant time.
func VerifyMAC(got, want []byte) bool { return hmac.Equal(got, want) }

// CertInfo identifies the certificate of an enrolled node.
type CertInfo struct {
	fingerprint [32]byte
	serial      string
	notAfter    time.Time
}

// NewCertInfo validates the parts of a certificate identity.
func NewCertInfo(fingerprint []byte, serial string, notAfter time.Time) (CertInfo, error) {
	var c CertInfo
	if len(fingerprint) != len(c.fingerprint) || serial == "" || len(serial) > 64 || notAfter.IsZero() {
		return CertInfo{}, ErrInvalidCertInfo
	}

	if _, err := hex.DecodeString(padHex(serial)); err != nil {
		return CertInfo{}, ErrInvalidCertInfo.WithDetail("serial must be hex")
	}

	copy(c.fingerprint[:], fingerprint)
	c.serial = strings.ToUpper(serial)
	c.notAfter = notAfter.UTC().Truncate(time.Millisecond)

	return c, nil
}

func padHex(s string) string {
	if len(s)%2 == 1 {
		return "0" + s
	}

	return s
}

// Fingerprint returns the SHA-256 of the certificate.
func (c CertInfo) Fingerprint() [32]byte { return c.fingerprint }

// FingerprintString renders the fingerprint as colon-separated upper-case
// hex.
func (c CertInfo) FingerprintString() string { return FormatFingerprint(c.fingerprint) }

// FormatFingerprint renders a SHA-256 fingerprint as colon-separated
// upper-case hex.
func FormatFingerprint(fp [32]byte) string {
	parts := make([]string, len(fp))
	for i, b := range fp {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}

	return strings.Join(parts, ":")
}

// Serial returns the serial in upper-case hex.
func (c CertInfo) Serial() string { return c.serial }

// NotAfter returns the end of validity.
func (c CertInfo) NotAfter() time.Time { return c.notAfter }

// IsZero reports an absent certificate.
func (c CertInfo) IsZero() bool { return c.serial == "" }
