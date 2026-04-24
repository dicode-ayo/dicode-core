package relay

import (
	"fmt"

	relaypb "github.com/dicode/dicode/pkg/relay/pb"
	"google.golang.org/protobuf/encoding/protojson"
)

// BrokerProtocolMin is the lowest broker protocol version this daemon will
// accept. Lower values mean the broker predates the current wire format
// (#104 split sign/decrypt keys + the #195 protobuf-es refactor). Version 3
// is mandatory — earlier brokers produced a different on-wire shape for
// `headers` and `timestamp` that the generated types cannot decode.
const BrokerProtocolMin = 3

// marshalOpts is a shared protojson configuration for all outbound messages.
// EmitUnpopulated: false keeps the on-wire JSON identical across builds — we
// don't want a zero-valued optional field to appear on the wire as a null or
// explicit empty.
var marshalOpts = protojson.MarshalOptions{
	UseProtoNames:   true, // emit fields as declared (snake_case in the proto)
	EmitUnpopulated: false,
}

// unmarshalOpts is permissive on unknown fields so that newer-relay → older-
// daemon hops don't fail hard when the relay adds a field the daemon doesn't
// yet know about. Mismatches on required fields still surface as errors.
var unmarshalOpts = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

// encodeClientMessage marshals a ClientMessage envelope to JSON bytes for
// sending over the WSS tunnel.
func encodeClientMessage(msg *relaypb.ClientMessage) ([]byte, error) {
	return marshalOpts.Marshal(msg)
}

// encodeServerMessage mirrors encodeClientMessage for server→daemon frames.
// Used only in tests in this package; production server-side code lives in
// dicode-relay.
func encodeServerMessage(msg *relaypb.ServerMessage) ([]byte, error) {
	return marshalOpts.Marshal(msg)
}

// decodeServerMessage unmarshals a WSS frame into a ServerMessage envelope.
// The caller dispatches on msg.GetKind().
func decodeServerMessage(data []byte) (*relaypb.ServerMessage, error) {
	var msg relaypb.ServerMessage
	if err := unmarshalOpts.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("decode server message: %w", err)
	}
	return &msg, nil
}

// headersFromHTTP converts a Go http.Header (map[string][]string) into the
// wire representation — each value slice wrapped in a HeaderValues message
// so it can be the value type of a proto3 map.
func headersFromHTTP(h map[string][]string) map[string]*relaypb.HeaderValues {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]*relaypb.HeaderValues, len(h))
	for k, vals := range h {
		out[k] = &relaypb.HeaderValues{Values: vals}
	}
	return out
}

// headersToHTTP is the inverse of headersFromHTTP, used on received Request
// messages when feeding the incoming headers into a Go http.Request.
func headersToHTTP(h map[string]*relaypb.HeaderValues) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, hv := range h {
		if hv != nil {
			out[k] = hv.Values
		}
	}
	return out
}
