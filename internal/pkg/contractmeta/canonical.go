package contractmeta

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Canonical produces the byte string a contract's token is computed over
// (§1.5: `canonical_json(body ∪ {kind, subject, version})`).
//
// The union is expressed as an explicit envelope rather than by merging keys
// into the body:
//
//	{"body":<canonical body>,"kind":"...","subject":"...","version":N}
//
// A literal merge is unsafe here: a contract body already carries its own
// `version` field, so merging would either collide or silently drop one of the
// two. The envelope keeps both unambiguous and is still one canonical document
// over exactly {body, kind, subject, version}.
//
// Canonicalisation rules, chosen so two encoders of the same value always agree:
//
//   - object keys sorted bytewise, every value encoded recursively
//   - no whitespace anywhere
//   - numbers reproduced VERBATIM from their JSON source (via json.Number).
//     Decoding into `any` without this yields float64, which cannot represent
//     an integer above 2^53 and switches to exponent form at 1e21 — either of
//     which would silently change a contract's token
//   - strings escaped by encoding/json
//
// The body is normalised through JSON first, so a struct, a map, or a
// json.RawMessage carrying the same content produce the same bytes, and Go map
// iteration order cannot leak into the result.
//
// Canonical never fails: a body that cannot be marshalled produces a document
// carrying an `_error` key, which can never equal a real body's canonical form,
// so a broken body can never accidentally verify.
func Canonical(body any, kind, subject string, version int) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')

	normalized, encErr := normalize(body)
	buf.WriteString(`"body":`)
	if encErr != nil {
		buf.WriteString("null")
	} else {
		writeCanonical(&buf, normalized)
	}

	if encErr != nil {
		buf.WriteString(`,"_error":`)
		writeString(&buf, encErr.Error())
	}

	buf.WriteString(`,"kind":`)
	writeString(&buf, kind)
	buf.WriteString(`,"subject":`)
	writeString(&buf, subject)
	buf.WriteString(`,"version":`)
	buf.WriteString(itoa(version))

	buf.WriteByte('}')
	return buf.Bytes()
}

// normalize marshals v and decodes it back with UseNumber, so everything below
// is one of: nil, bool, string, json.Number, []any, map[string]any.
func normalize(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(t.String())
	case string:
		writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, e)
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, t[k])
		}
		buf.WriteByte('}')
	default:
		// Unreachable after normalize; encoded defensively rather than dropped.
		raw, err := json.Marshal(t)
		if err != nil {
			buf.WriteString("null")
			return
		}
		buf.Write(raw)
	}
}

func writeString(buf *bytes.Buffer, s string) {
	raw, err := json.Marshal(s)
	if err != nil {
		buf.WriteString(`""`)
		return
	}
	buf.Write(raw)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d [24]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		d[i] = '-'
	}
	return string(d[i:])
}
