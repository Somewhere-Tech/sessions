package proto

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// ModelControl changes the defaults used by the next structured-provider turn.
// It travels in a protocol-v2 request frame and is acknowledged by the runner
// only after its provider state and durable metadata both accept the change.
type ModelControl struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type ModelControlResult struct {
	Error string `json:"error,omitempty"`
}

func EncodeModelControl(control ModelControl) ([]byte, error) {
	return json.Marshal(control)
}

func DecodeModelControl(data []byte) (ModelControl, error) {
	var control ModelControl
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return ModelControl{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ModelControl{}, errors.New("model control contains trailing JSON")
		}
		return ModelControl{}, err
	}
	return control, nil
}
