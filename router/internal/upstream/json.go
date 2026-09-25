package upstream

import (
	"bytes"
	"encoding/json"
)

// object is a JSON object whose values stay as the bytes they came in, so
// only what the router changes is re-encoded.
type object map[string]json.RawMessage

func decode(data []byte) (object, error) {
	var o object
	err := json.Unmarshal(data, &o)
	return o, err
}

// str is the string at key, or "" when it is missing or not a string.
func (o object) str(key string) string {
	var s string
	json.Unmarshal(o[key], &s)
	return s
}

// encode marshals values that always encode, keeping < > & as they are.
func encode(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
