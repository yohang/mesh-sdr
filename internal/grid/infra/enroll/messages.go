// Package enroll implements the enrollment exchange of TECHNICAL_SPEC §4.2
// over HTTPS (ADR 0008 Q11–Q13): the hub dials POST https://<node>/enroll,
// trusting only the node's self-signed certificate for this exchange, in
// two phases bound by a nonce and HMACs keyed by the enrollment key.
package enroll

import (
	"encoding/base64"
	"errors"
	"fmt"
)

// Phases of POST /enroll.
const (
	PhaseHello       = "hello"
	PhaseCertificate = "certificate"
)

// MaxBody bounds request and response bodies.
const MaxBody = 64 << 10

// request is the body of POST /enroll in both phases.
type request struct {
	Phase string `json:"phase"`

	// Phase hello.
	NodeID     string   `json:"node_id,omitempty"`
	HubCAChain []string `json:"hub_ca_chain,omitempty"`
	Proof      string   `json:"proof,omitempty"`

	// Both phases.
	Nonce string `json:"nonce"`

	// Phase certificate.
	CertificateChain []string `json:"certificate_chain,omitempty"`
	MAC              string   `json:"mac,omitempty"`
}

// helloResponse answers phase hello.
type helloResponse struct {
	CSR string `json:"csr"`
	MAC string `json:"mac"`
}

var b64 = base64.StdEncoding

func encodeAll(ders [][]byte) []string {
	out := make([]string, len(ders))
	for i, d := range ders {
		out[i] = b64.EncodeToString(d)
	}

	return out
}

func decodeAll(s []string) ([][]byte, error) {
	if len(s) == 0 || len(s) > 4 {
		return nil, errors.New("want 1 to 4 certificates")
	}

	out := make([][]byte, len(s))

	for i, v := range s {
		b, err := b64.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", i, err)
		}

		out[i] = b
	}

	return out, nil
}
