package vai

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

var defaultToolArgStringKeys = []string{"content", "message", "text"}

// ToolArgStringDecoderOptions configures ToolArgStringDecoder behavior.
type ToolArgStringDecoderOptions struct {
	Keys []string
}

// ToolArgStringUpdate is the result of pushing a new partial JSON fragment.
type ToolArgStringUpdate struct {
	Found bool
	Key   string
	Full  string
	Delta string
}

// ToolArgStringDecoder incrementally decodes a root-level string field from
// streamed tool argument JSON fragments.
type ToolArgStringDecoder struct {
	keys       []string
	raw        strings.Builder
	matchedKey string
	lastFull   string
}

// NewToolArgStringDecoder creates a new incremental tool-arg string decoder.
func NewToolArgStringDecoder(opts ToolArgStringDecoderOptions) *ToolArgStringDecoder {
	return &ToolArgStringDecoder{
		keys: normalizeToolArgStringKeys(opts.Keys),
	}
}

// Push appends a raw JSON fragment and returns decoded incremental state.
func (d *ToolArgStringDecoder) Push(partialJSON string) ToolArgStringUpdate {
	if d == nil {
		return ToolArgStringUpdate{}
	}
	if partialJSON != "" {
		d.raw.WriteString(partialJSON)
	}

	raw := d.raw.String()
	if raw == "" {
		return ToolArgStringUpdate{}
	}

	if d.matchedKey == "" {
		key, full, found := extractFirstRootStringFieldPrefix(raw, d.keys)
		if !found {
			return ToolArgStringUpdate{}
		}
		d.matchedKey = key
		delta := decodedDelta(d.lastFull, full)
		d.lastFull = full
		return ToolArgStringUpdate{
			Found: true,
			Key:   key,
			Full:  full,
			Delta: delta,
		}
	}

	full, found := extractRootStringFieldPrefix(raw, d.matchedKey)
	if !found {
		return ToolArgStringUpdate{}
	}
	delta := decodedDelta(d.lastFull, full)
	d.lastFull = full
	return ToolArgStringUpdate{
		Found: true,
		Key:   d.matchedKey,
		Full:  full,
		Delta: delta,
	}
}

// Reset clears accumulated stream state but preserves configured keys.
func (d *ToolArgStringDecoder) Reset() {
	if d == nil {
		return
	}
	d.raw.Reset()
	d.matchedKey = ""
	d.lastFull = ""
}

// MatchedKey returns the currently locked key, if any.
func (d *ToolArgStringDecoder) MatchedKey() string {
	if d == nil {
		return ""
	}
	return d.matchedKey
}

func normalizeToolArgStringKeys(keys []string) []string {
	if len(keys) == 0 {
		out := make([]string, len(defaultToolArgStringKeys))
		copy(out, defaultToolArgStringKeys)
		return out
	}

	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	if len(out) == 0 {
		out = make([]string, len(defaultToolArgStringKeys))
		copy(out, defaultToolArgStringKeys)
	}
	return out
}

func extractFirstRootStringFieldPrefix(raw string, keys []string) (key string, decoded string, found bool) {
	if raw == "" || len(keys) == 0 {
		return "", "", false
	}
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	key, valueStart, found := findRootStringFieldValueStart(raw, allowed)
	if !found {
		return "", "", false
	}
	return key, decodeJSONStringPrefixFrom(raw[valueStart:]), true
}

func extractRootStringFieldPrefix(raw string, key string) (decoded string, found bool) {
	if raw == "" || key == "" {
		return "", false
	}
	allowed := map[string]struct{}{key: {}}
	foundKey, valueStart, ok := findRootStringFieldValueStart(raw, allowed)
	if !ok || foundKey == "" {
		return "", false
	}
	return decodeJSONStringPrefixFrom(raw[valueStart:]), true
}

func findRootStringFieldValueStart(raw string, allowed map[string]struct{}) (key string, valueStart int, found bool) {
	if raw == "" || len(allowed) == 0 {
		return "", 0, false
	}

	depth := 0
	for i := 0; i < len(raw); {
		switch raw[i] {
		case '"':
			decoded, end, complete := parseJSONStringToken(raw, i)
			if !complete {
				return "", 0, false
			}
			if depth != 1 {
				i = end
				continue
			}
			j := skipJSONWhitespace(raw, end)
			if j >= len(raw) || raw[j] != ':' {
				i = end
				continue
			}
			j++
			j = skipJSONWhitespace(raw, j)
			if j >= len(raw) {
				return "", 0, false
			}
			if _, ok := allowed[decoded]; !ok {
				i = end
				continue
			}
			if raw[j] != '"' {
				i = end
				continue
			}
			return decoded, j + 1, true
		case '{', '[':
			depth++
			i++
		case '}', ']':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}

	return "", 0, false
}

func parseJSONStringToken(raw string, start int) (decoded string, end int, complete bool) {
	if start < 0 || start >= len(raw) || raw[start] != '"' {
		return "", start, false
	}

	var out strings.Builder
	for i := start + 1; i < len(raw); {
		ch := raw[i]
		if ch == '"' {
			return out.String(), i + 1, true
		}
		if ch != '\\' {
			out.WriteByte(ch)
			i++
			continue
		}

		if i+1 >= len(raw) {
			return out.String(), i, false
		}
		esc := raw[i+1]
		switch esc {
		case '"', '\\', '/':
			out.WriteByte(esc)
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			r, consumed, ok := decodeUnicodeEscape(raw, i)
			if !ok {
				return out.String(), i, false
			}
			out.WriteRune(r)
			i += consumed
		default:
			return out.String(), i, false
		}
	}
	return out.String(), len(raw), false
}

func decodeJSONStringPrefixFrom(raw string) string {
	var out strings.Builder
	for i := 0; i < len(raw); {
		ch := raw[i]
		if ch == '"' {
			return out.String()
		}
		if ch != '\\' {
			out.WriteByte(ch)
			i++
			continue
		}
		if i+1 >= len(raw) {
			return out.String()
		}
		esc := raw[i+1]
		switch esc {
		case '"', '\\', '/':
			out.WriteByte(esc)
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			r, consumed, ok := decodeUnicodeEscape(raw, i)
			if !ok {
				return out.String()
			}
			out.WriteRune(r)
			i += consumed
		default:
			return out.String()
		}
	}
	return out.String()
}

func decodeUnicodeEscape(raw string, slashIndex int) (rune, int, bool) {
	// Sequence begins at raw[slashIndex] == '\\' and raw[slashIndex+1] == 'u'
	if slashIndex+5 >= len(raw) || raw[slashIndex] != '\\' || raw[slashIndex+1] != 'u' {
		return 0, 0, false
	}
	r1, ok := parseHexRune(raw[slashIndex+2 : slashIndex+6])
	if !ok {
		return 0, 0, false
	}

	if isHighSurrogate(r1) {
		// Need a complete low-surrogate pair to emit a rune.
		if slashIndex+11 >= len(raw) {
			return 0, 0, false
		}
		if raw[slashIndex+6] != '\\' || raw[slashIndex+7] != 'u' {
			return 0, 0, false
		}
		r2, ok := parseHexRune(raw[slashIndex+8 : slashIndex+12])
		if !ok || !isLowSurrogate(r2) {
			return 0, 0, false
		}
		return utf16.DecodeRune(r1, r2), 12, true
	}
	if isLowSurrogate(r1) {
		return 0, 0, false
	}
	return r1, 6, true
}

func parseHexRune(s string) (rune, bool) {
	if len(s) != 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

func isHighSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDBFF
}

func isLowSurrogate(r rune) bool {
	return r >= 0xDC00 && r <= 0xDFFF
}

func skipJSONWhitespace(raw string, pos int) int {
	for pos < len(raw) {
		switch raw[pos] {
		case ' ', '\t', '\n', '\r':
			pos++
		default:
			return pos
		}
	}
	return pos
}

func decodedDelta(previous string, current string) string {
	if previous == "" {
		return current
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	common := commonPrefixLen(previous, current)
	if common >= len(current) {
		return ""
	}
	return current[common:]
}

func commonPrefixLen(a string, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	i := 0
	for i < limit && a[i] == b[i] {
		i++
	}
	return i
}
