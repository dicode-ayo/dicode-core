package relay

import "encoding/json"

const (
	msgChallenge = "challenge"
	msgHello     = "hello"
	msgWelcome   = "welcome"
	msgError     = "error"
	msgRequest   = "request"
	msgResponse  = "response"
)

type challengeMsg struct {
	Type  string `json:"type"`
	Nonce string `json:"nonce"`
}

type helloMsg struct {
	Type string `json:"type"`
	UUID string `json:"uuid"`
	// PubKey is the base64-encoded uncompressed SignKey public key (65 bytes).
	// The broker uses it both to verify the ECDSA handshake signature and —
	// on pre-#104 brokers — as the ECIES recipient for OAuth deliveries.
	PubKey string `json:"pubkey"`
	// DecryptPubKey is the base64-encoded uncompressed DecryptKey public key
	// (65 bytes). Post-#104 brokers encrypt OAuth deliveries against this
	// key instead of PubKey. The field is always populated by post-#104
	// daemons; pre-#104 brokers ignore it and fall back to PubKey.
	DecryptPubKey string `json:"decrypt_pubkey,omitempty"`
	Sig           string `json:"sig"`
	Timestamp     int64  `json:"timestamp"`
}

type welcomeMsg struct {
	Type         string `json:"type"`
	URL          string `json:"url"`
	BrokerPubkey string `json:"broker_pubkey,omitempty"` // base64 SPKI DER — TOFU-pinned by the daemon
	// Protocol is the broker's wire-protocol version. A value >= 2 means the
	// broker understands the split sign/decrypt key scheme (issue #104) and
	// will encrypt OAuth deliveries to the daemon's DecryptKey pubkey.
	// Protocol 1 (or an absent field) means the broker is pre-#104 and the
	// daemon must refuse OAuth IPC flows to avoid silent decrypt failures.
	Protocol int `json:"protocol,omitempty"`
}

type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type requestMsg struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64
}

type responseMsg struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64
}

func encodeMsg(v any) ([]byte, error) {
	return json.Marshal(v)
}

func msgType(data []byte) string {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &m)
	return m.Type
}
