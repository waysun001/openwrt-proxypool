// Package api implements the bounded, local control protocol used by proxypoold.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ProtocolVersion = 1
	// MaxFrameSize is the maximum number of JSON bytes before the mandatory LF.
	MaxFrameSize = 1 << 20
)

// Request is the protocol envelope. Params deliberately has no useful string
// representation: it can contain credentials and must not leak through logging.
type Request struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (r Request) String() string {
	return fmt.Sprintf("api.Request{Version:%d ID:%q Method:%q Params:<redacted>}", r.Version, r.ID, r.Method)
}
func (r Request) GoString() string { return r.String() }
func (r Request) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, "api.Request{Params:<redacted>}")
}

// Response is always emitted as one JSON line by Server and proxypoolctl.
type Response struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the stable machine-readable API failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var errInvalidRequest = errors.New("invalid request")

type requestWire struct {
	Version *int             `json:"version"`
	ID      *string          `json:"id"`
	Method  *string          `json:"method"`
	Params  *json.RawMessage `json:"params"`
}

type responseWire struct {
	Version *int             `json:"version"`
	ID      *string          `json:"id"`
	Result  *json.RawMessage `json:"result"`
	Error   *Error           `json:"error"`
}

// ParseRequest validates one complete JSON object. It is intentionally strict:
// unknown fields, duplicate object keys, absent envelope fields, and trailing
// JSON are all invalid.
func ParseRequest(data []byte) (Request, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return Request{}, errInvalidRequest
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire requestWire
	if err := dec.Decode(&wire); err != nil {
		return Request{}, errInvalidRequest
	}
	if err := requireEOF(dec); err != nil || wire.Version == nil || wire.ID == nil || wire.Method == nil || wire.Params == nil || *wire.Method == "" {
		return Request{}, errInvalidRequest
	}
	return Request{Version: *wire.Version, ID: *wire.ID, Method: *wire.Method, Params: append(json.RawMessage(nil), (*wire.Params)...)}, nil
}

func parseResponse(data []byte) (Response, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return Response{}, errInvalidRequest
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire responseWire
	if err := dec.Decode(&wire); err != nil {
		return Response{}, errInvalidRequest
	}
	if err := requireEOF(dec); err != nil || wire.Version == nil || wire.ID == nil || (wire.Result == nil && wire.Error == nil) || (wire.Result != nil && wire.Error != nil) {
		return Response{}, errInvalidRequest
	}
	response := Response{Version: *wire.Version, ID: *wire.ID, Error: wire.Error}
	if wire.Result != nil {
		response.Result = append(json.RawMessage(nil), (*wire.Result)...)
	}
	if response.Error != nil && (response.Error.Code == "" || response.Error.Message == "") {
		return Response{}, errInvalidRequest
	}
	return response, nil
}

func requireEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	}
	return errInvalidRequest
}

func requestID(data []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&envelope) != nil {
		return partialRequestID(data)
	}
	return envelope.ID
}

// partialRequestID preserves correlation for a syntactically broken envelope
// once its top-level id value has already been parsed. It never inspects Params.
func partialRequestID(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}
	var id string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		name, ok := key.(string)
		if !ok {
			break
		}
		if name == "id" {
			var value string
			if err := dec.Decode(&value); err != nil {
				break
			}
			id = value
			continue
		}
		var discard any
		if err := dec.Decode(&discard); err != nil {
			break
		}
	}
	return id
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(dec); err != nil {
		return err
	}
	_, err := dec.Token()
	if err != io.EOF {
		return errInvalidRequest
	}
	return nil
}

func scanJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errInvalidRequest
			}
			if _, exists := seen[name]; exists {
				return errInvalidRequest
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return errInvalidRequest
	}
}
