package protocol

import (
	"regexp"
	"time"
)

const Version uint32 = 1

type MessageType string

const (
	MessageConnectorHello MessageType = "connector.hello"
	MessageHeartbeat      MessageType = "connector.heartbeat"
	MessageAck            MessageType = "hub.ack"
	MessageError          MessageType = "hub.error"
	MessageRPCRequest     MessageType = "rpc.request"
	MessageRPCResponse    MessageType = "rpc.response"
	MessageStreamOpen     MessageType = "stream.open"
	MessageStreamData     MessageType = "stream.data"
	MessageStreamResize   MessageType = "stream.resize"
	MessageStreamClose    MessageType = "stream.close"
)

type Envelope struct {
	Version      uint32          `json:"version"`
	Type         MessageType     `json:"type"`
	RequestID    string          `json:"requestId,omitempty"`
	Hello        *ConnectorHello `json:"hello,omitempty"`
	Heartbeat    *Heartbeat      `json:"heartbeat,omitempty"`
	Ack          *Ack            `json:"ack,omitempty"`
	Error        *Error          `json:"error,omitempty"`
	Request      *RPCRequest     `json:"request,omitempty"`
	Response     *RPCResponse    `json:"response,omitempty"`
	StreamOpen   *StreamOpen     `json:"streamOpen,omitempty"`
	StreamData   *StreamData     `json:"streamData,omitempty"`
	StreamResize *StreamResize   `json:"streamResize,omitempty"`
	StreamClose  *StreamClose    `json:"streamClose,omitempty"`
}

type ConnectorHello struct {
	DeviceID         string   `json:"deviceId"`
	Name             string   `json:"name"`
	Platform         string   `json:"platform"`
	Architecture     string   `json:"architecture"`
	ConnectorVersion string   `json:"connectorVersion"`
	Capabilities     []string `json:"capabilities"`
}

type Heartbeat struct {
	DeviceID string    `json:"deviceId"`
	SentAt   time.Time `json:"sentAt"`
}

type Ack struct {
	Message string `json:"message"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RPCRequest struct {
	Method  string `json:"method"`
	Name    string `json:"name,omitempty"`
	NewName string `json:"newName,omitempty"`
}

type RPCResponse struct {
	OK        bool          `json:"ok"`
	Sessions  []TmuxSession `json:"sessions,omitempty"`
	Error     string        `json:"error,omitempty"`
	ErrorCode string        `json:"errorCode,omitempty"`
	Name      string        `json:"name,omitempty"`
}

var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ValidSessionName(name string) bool {
	return sessionNamePattern.MatchString(name)
}

type TmuxSession struct {
	Name      string `json:"name"`
	Windows   int    `json:"windows"`
	Attached  bool   `json:"attached"`
	CreatedAt int64  `json:"createdAt"`
}

type StreamOpen struct {
	StreamID string `json:"streamId"`
	Session  string `json:"session"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
}

type StreamData struct {
	StreamID string `json:"streamId"`
	Data     []byte `json:"data"`
}

type StreamResize struct {
	StreamID string `json:"streamId"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
}

type StreamClose struct {
	StreamID string `json:"streamId"`
	Reason   string `json:"reason,omitempty"`
}
