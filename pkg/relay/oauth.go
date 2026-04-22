package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

// hkdfInfo is the fixed info string used when deriving the AES key for
// OAuth token delivery. It must match the relay broker
// (see dicode-relay src/broker/crypto.ts).
const hkdfInfo = "dicode-oauth-token"

// AuthRequest is the data the daemon needs to track for a pending OAuth
// flow. The challenge is retained only because it's part of the signed
// payload the broker verifies — the daemon never runs the code exchange
// itself (Grant on the relay handles PKCE upstream), so no verifier is
// stored here.
type AuthRequest struct {
	Provider      string
	SessionID     string // UUID v4, with dashes
	PKCEChallenge string // base64url(sha256(random verifier)) — bound into the signed payload
	Timestamp     int64  // unix seconds
}

// BuildAuthSignedPayload returns the byte sequence the daemon must sign when
// initiating an OAuth flow via the broker. It must mirror buildSignedPayload()
// in dicode-relay src/broker/crypto.ts exactly:
//
//	sha256(
//	  session_id_bytes      // 16 bytes, UUID v4 hex-decoded (dashes stripped)
//	  pkce_challenge_bytes  // base64url-decoded
//	  relay_uuid_bytes      // 32 bytes, hex-decoded
//	  provider_utf8_bytes
//	  timestamp_be_uint64   // 8 bytes
//	)
func BuildAuthSignedPayload(sessionID, pkceChallenge, relayUUID, provider string, timestamp int64) ([]byte, error) {
	sidHex := strings.ReplaceAll(sessionID, "-", "")
	sidBytes, err := hex.DecodeString(sidHex)
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	if len(sidBytes) != 16 {
		return nil, fmt.Errorf("session id must decode to 16 bytes, got %d", len(sidBytes))
	}

	challengeBytes, err := base64.RawURLEncoding.DecodeString(pkceChallenge)
	if err != nil {
		return nil, fmt.Errorf("pkce challenge: %w", err)
	}

	relayBytes, err := hex.DecodeString(relayUUID)
	if err != nil {
		return nil, fmt.Errorf("relay uuid: %w", err)
	}
	if len(relayBytes) != 32 {
		return nil, fmt.Errorf("relay uuid must decode to 32 bytes, got %d", len(relayBytes))
	}

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(timestamp))

	h := sha256.New()
	h.Write(sidBytes)
	h.Write(challengeBytes)
	h.Write(relayBytes)
	h.Write([]byte(provider))
	h.Write(ts[:])
	return h.Sum(nil), nil
}

// SignAuthPayload signs the precomputed payload with the daemon's ECDSA P-256
// private key and returns the standard-base64-encoded ASN.1 DER signature.
//
// The signing shape is:  ecdsa.SignASN1(priv, sha256(payload))
//
// i.e., the 32-byte payload (already sha256(fields) from BuildAuthSignedPayload)
// is hashed ONE MORE TIME before signing. This matches Node's
// `createSign("SHA256").update(payload).sign()` on the broker-verify side —
// Node's createVerify internally hashes its update input before comparing
// against the embedded digest, so the broker expects a sig over
// sha256(sha256(fields)). Mirrors the symmetric fix on the broker-sig
// delivery path (dicode-core#152 / dicode-relay#152).
func SignAuthPayload(priv *ecdsa.PrivateKey, payload []byte) (string, error) {
	outer := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, outer[:])
	if err != nil {
		return "", fmt.Errorf("ecdsa sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// generatePKCEChallenge returns a fresh base64url S256 challenge. The
// underlying verifier is intentionally discarded: the daemon never completes
// the code exchange itself — Grant on the relay side runs its own PKCE
// against the upstream provider. Our challenge exists solely to bind the
// daemon's signed payload to a value that cannot be swapped in the URL.
func generatePKCEChallenge() (string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// BuildAuthURL constructs a signed `/auth/:provider` URL pointing at the
// relay broker and returns the matching AuthRequest for the daemon-side
// pending store.
//
//	baseURL  e.g. "https://relay.dicode.app"
//	scope    optional space-separated scope override (empty = use broker default)
func BuildAuthURL(baseURL string, identity *Identity, provider, scope string, now int64) (string, *AuthRequest, error) {
	if identity == nil || identity.SignKey == nil {
		return "", nil, fmt.Errorf("identity required")
	}
	if provider == "" {
		return "", nil, fmt.Errorf("provider required")
	}

	challenge, err := generatePKCEChallenge()
	if err != nil {
		return "", nil, err
	}

	sessionID := uuid.NewString()

	payload, err := BuildAuthSignedPayload(sessionID, challenge, identity.UUID, provider, now)
	if err != nil {
		return "", nil, err
	}
	sig, err := SignAuthPayload(identity.SignKey, payload)
	if err != nil {
		return "", nil, err
	}

	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/auth/" + url.PathEscape(provider))
	if err != nil {
		return "", nil, fmt.Errorf("parse base url: %w", err)
	}
	q := u.Query()
	q.Set("session", sessionID)
	q.Set("challenge", challenge)
	q.Set("relay_uuid", identity.UUID)
	q.Set("sig", sig)
	q.Set("timestamp", fmt.Sprintf("%d", now))
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	return u.String(), &AuthRequest{
		Provider:      provider,
		SessionID:     sessionID,
		PKCEChallenge: challenge,
		Timestamp:     now,
	}, nil
}

// OAuthDeliveryType is the wire-format type tag on token delivery envelopes.
// It is used both as the explicit guard in DecryptOAuthToken and as AES-GCM
// AAD on both ends (relay: eciesEncrypt, daemon: aead.Open). Keep in sync
// with dicode-relay src/broker/crypto.ts.
const OAuthDeliveryType = "oauth_token_delivery"

// OAuthTokenDeliveryPayload is the JSON body the broker POSTs to
// /hooks/oauth-complete on the daemon, wrapped in the relay request envelope.
// It mirrors OAuthTokenDeliveryPayload in dicode-relay src/relay/protocol.ts.
type OAuthTokenDeliveryPayload struct {
	Type            string `json:"type"`                 // must equal OAuthDeliveryType
	SessionID       string `json:"session_id"`           // matches the original AuthRequest
	EphemeralPubkey string `json:"ephemeral_pubkey"`     // base64 std, 65-byte uncompressed P-256
	Ciphertext      string `json:"ciphertext"`           // base64 std, AES-256-GCM ct || 16-byte tag
	Nonce           string `json:"nonce"`                // base64 std, 12 bytes
	BrokerSig       string `json:"broker_sig,omitempty"` // base64 ECDSA sig over sha256(type||session_id||eph||ct||nonce)
}

// DecryptOAuthToken decrypts an OAuthTokenDeliveryPayload using the daemon's
// long-lived private key. It performs ECDH against the broker's ephemeral
// public key, derives a 32-byte AES-256 key via HKDF-SHA256 (with the session
// id as salt and "dicode-oauth-token" as info), and finally AES-256-GCM
// decrypts the payload. The 16-byte GCM auth tag is appended to the ciphertext
// per the broker convention; this function splits it back off.
func DecryptOAuthToken(identity *Identity, payload *OAuthTokenDeliveryPayload) ([]byte, error) {
	if identity == nil || identity.DecryptKey == nil {
		return nil, fmt.Errorf("identity required")
	}
	if payload == nil {
		return nil, fmt.Errorf("payload required")
	}
	// Require an explicit, known type tag. The tag is also bound into the
	// GCM authenticated data below, so a mismatch (or tampering in transit)
	// makes aead.Open fail. This is domain separation: the daemon identity
	// key can never be asked to decrypt a future ciphertext that reuses
	// this same ECIES scheme under a different Type label.
	if payload.Type != OAuthDeliveryType {
		return nil, fmt.Errorf("unexpected payload type: %q", payload.Type)
	}

	ephBytes, err := base64.StdEncoding.DecodeString(payload.EphemeralPubkey)
	if err != nil {
		return nil, fmt.Errorf("decode ephemeral pubkey: %w", err)
	}
	curve := ecdh.P256()
	ephPub, err := curve.NewPublicKey(ephBytes)
	if err != nil {
		return nil, fmt.Errorf("parse ephemeral pubkey: %w", err)
	}

	// Convert the long-lived DecryptKey (ECDSA type, P-256 curve) to an ECDH
	// key so it can participate in ECDH against the broker's ephemeral key.
	// #104: the SignKey must never take this code path — ECDSA-only domain
	// separation is exactly what the split buys us.
	daemonECDH, err := ecdsaToECDH(identity.DecryptKey)
	if err != nil {
		return nil, fmt.Errorf("convert daemon key: %w", err)
	}

	shared, err := daemonECDH.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}

	encKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, shared, []byte(payload.SessionID), []byte(hkdfInfo))
	if _, err := io.ReadFull(kdf, encKey); err != nil {
		return nil, fmt.Errorf("hkdf: %w", err)
	}

	iv, err := base64.StdEncoding.DecodeString(payload.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	if len(iv) != 12 {
		return nil, fmt.Errorf("nonce must be 12 bytes, got %d", len(iv))
	}

	ctWithTag, err := base64.StdEncoding.DecodeString(payload.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	if len(ctWithTag) < 16 {
		return nil, fmt.Errorf("ciphertext too short for gcm tag")
	}

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	// crypto/cipher GCM expects ct || tag, which matches the broker layout.
	// Type is passed as AAD — the broker binds the same bytes on encrypt,
	// so any mismatch (or in-transit tampering of Type) makes Open fail.
	pt, err := aead.Open(nil, iv, ctWithTag, []byte(payload.Type))
	if err != nil {
		return nil, fmt.Errorf("aes-gcm open: %w", err)
	}
	return pt, nil
}

// VerifyBrokerSig verifies the broker's ECDSA signature over the delivery
// envelope's immutable fields. brokerPubkeyBase64 is the base64-encoded SPKI
// DER public key received (and TOFU-pinned) from the relay's welcome message.
//
// The signed payload is sha256(type || session_id || ephemeral_pubkey ||
// ciphertext || nonce) — identical concatenation order on both sides.
func VerifyBrokerSig(brokerPubkeyBase64 string, payload *OAuthTokenDeliveryPayload) error {
	if brokerPubkeyBase64 == "" {
		return fmt.Errorf("no broker pubkey pinned — cannot verify delivery authenticity")
	}
	if payload.BrokerSig == "" {
		return fmt.Errorf("delivery envelope missing broker_sig — broker may be outdated")
	}
	pubDER, err := base64.StdEncoding.DecodeString(brokerPubkeyBase64)
	if err != nil {
		return fmt.Errorf("decode broker pubkey: %w", err)
	}

	// Reconstruct the exact digest the broker signed.
	//
	// The Node broker builds a signature payload via
	// dicode-relay/src/broker/signing.ts:buildDeliverySignaturePayload
	// — which returns sha256(type || session || eph || ct || nonce) as
	// 32 bytes — and then passes THAT to createSign("SHA256").update().sign().
	// Node's createSign SHA-256-hashes its input again before signing, so
	// the actual signed digest is sha256(sha256(fields)).
	//
	// ecdsa.VerifyASN1 treats its third argument as the already-hashed
	// message to verify against, so we must hand it the two-layer digest
	// to match. Handing the one-layer inner digest would verify against
	// sha256(fields) while Node signed sha256(sha256(fields)) — mismatch,
	// verification always fails. See #151 for the full trace.
	innerH := sha256.New()
	innerH.Write([]byte(payload.Type))
	innerH.Write([]byte(payload.SessionID))
	innerH.Write([]byte(payload.EphemeralPubkey))
	innerH.Write([]byte(payload.Ciphertext))
	innerH.Write([]byte(payload.Nonce))
	outerH := sha256.New()
	outerH.Write(innerH.Sum(nil))
	digest := outerH.Sum(nil)

	sigBytes, err := base64.StdEncoding.DecodeString(payload.BrokerSig)
	if err != nil {
		return fmt.Errorf("decode broker sig: %w", err)
	}

	// Parse the SPKI DER public key.
	pub, err := x509.ParsePKIXPublicKey(pubDER)
	if err != nil {
		return fmt.Errorf("parse broker pubkey: %w", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("broker pubkey is not ECDSA")
	}

	if !ecdsa.VerifyASN1(ecPub, digest, sigBytes) {
		return fmt.Errorf("broker signature verification failed — envelope may have been tampered with or forged")
	}
	return nil
}

// ecdsaToECDH converts a P-256 ECDSA private key to a crypto/ecdh private key
// so it can participate in ECDH against the broker's ephemeral key.
func ecdsaToECDH(priv *ecdsa.PrivateKey) (*ecdh.PrivateKey, error) {
	if name := priv.PublicKey.Curve.Params().Name; name != "P-256" {
		return nil, fmt.Errorf("only P-256 supported, got %s", name)
	}
	return ecdh.P256().NewPrivateKey(priv.D.FillBytes(make([]byte, 32)))
}
