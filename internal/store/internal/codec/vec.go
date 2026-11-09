// Package codec holds low-level encode/decode helpers shared by the sqlite and
// postgres store adapters.
package codec

import (
	"encoding/binary"
	"encoding/json"
	"math"
)

// EncodeVec serializes a float32 vector as little-endian bytes (the wire format
// for both the SQLite BLOB column and the pgvector binary protocol).
func EncodeVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// DecodeVec deserializes a float32 vector from little-endian bytes.
func DecodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// ExtractName returns the "name" field from a JSON props blob, or "" on any
// error or absence.
func ExtractName(props json.RawMessage) string {
	if len(props) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(props, &m); err != nil {
		return ""
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}
