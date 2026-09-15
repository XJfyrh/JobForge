// Command executorprobe exercises an isolated Go/Python process boundary.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	maxRequest  = 4096
	maxResponse = 16384
)

var (
	errProtocol    = errors.New("invalid executor protocol")
	errOutputLimit = errors.New("executor output limit exceeded")
)

type request struct {
	Version   int    `json:"v"`
	ID        string `json:"id"`
	Operation string `json:"op"`
	Value     string `json:"value"`
}

type response struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	PID     int    `json:"pid,omitempty"`
	Value   string `json:"value,omitempty"`
}

func encodeRequest(r request) ([]byte, error) {
	switch r.Operation {
	case "echo", "block", "ignore_term", "bad_json", "wrong_id", "oversize", "stderr", "trailing_bytes":
	default:
		return nil, errProtocol
	}
	if r.Version != 1 || len(r.ID) == 0 || len(r.ID) > 64 {
		return nil, errProtocol
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode executor request: %w", err)
	}
	if len(data)+1 > maxRequest {
		return nil, errOutputLimit
	}
	return append(data, '\n'), nil
}

func decodeResponse(data []byte, id string) (response, error) {
	var r response
	if len(data) > maxResponse {
		return r, errOutputLimit
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, errProtocol
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return r, errProtocol
	}
	if r.Version != 1 || r.ID != id {
		return r, errProtocol
	}
	if (r.Kind == "started" && r.PID > 0 && r.Value == "") ||
		(r.Kind == "result" && r.PID == 0) {
		return r, nil
	}
	return r, errProtocol
}
