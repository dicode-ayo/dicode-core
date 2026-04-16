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
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	PubKey    string `json:"pubkey"`
	Sig       string `json:"sig"`
	Timestamp int64  `json:"timestamp"`
}

type welcomeMsg struct {
	Type         string `json:"type"`
	URL          string `json:"url"`
	BrokerPubkey string `json:"broker_pubkey,omitempty"` // base64 SPKI DER — TOFU-pinned by the daemon
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
