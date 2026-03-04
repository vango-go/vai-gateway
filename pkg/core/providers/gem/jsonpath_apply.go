package gem

import (
	"fmt"
	"strconv"
	"strings"
)

type jsonPathSegment struct {
	key   string
	index int
	isKey bool
}

func applyPartialArgPath(root map[string]any, jsonPath string, value any) error {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "$" {
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("root JSONPath assignment requires object value")
		}
		for k := range root {
			delete(root, k)
		}
		for k, v := range m {
			root[k] = v
		}
		return nil
	}
	segments, err := parseJSONPath(jsonPath)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return fmt.Errorf("empty JSONPath")
	}
	updated, err := setAtPath(root, segments, value)
	if err != nil {
		return err
	}
	m, ok := updated.(map[string]any)
	if !ok {
		return fmt.Errorf("root JSONPath target must remain object")
	}
	// setAtPath may return the same map instance as root; copy first to avoid
	// deleting keys before we can reassign them.
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	for k := range root {
		delete(root, k)
	}
	for k, v := range cp {
		root[k] = v
	}
	return nil
}

func setAtPath(node any, segments []jsonPathSegment, value any) (any, error) {
	if len(segments) == 0 {
		return value, nil
	}
	seg := segments[0]
	rest := segments[1:]

	if seg.isKey {
		var m map[string]any
		switch n := node.(type) {
		case nil:
			m = map[string]any{}
		case map[string]any:
			m = n
		default:
			return nil, fmt.Errorf("expected object for key %q", seg.key)
		}
		child := m[seg.key]
		next, err := setAtPath(child, rest, value)
		if err != nil {
			return nil, err
		}
		m[seg.key] = next
		return m, nil
	}

	var arr []any
	switch n := node.(type) {
	case nil:
		arr = make([]any, seg.index+1)
	case []any:
		arr = n
	default:
		return nil, fmt.Errorf("expected array for index %d", seg.index)
	}
	if seg.index < 0 {
		return nil, fmt.Errorf("negative array index %d", seg.index)
	}
	if len(arr) <= seg.index {
		ext := make([]any, seg.index+1)
		copy(ext, arr)
		arr = ext
	}
	next, err := setAtPath(arr[seg.index], rest, value)
	if err != nil {
		return nil, err
	}
	arr[seg.index] = next
	return arr, nil
}

func parseJSONPath(path string) ([]jsonPathSegment, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("JSONPath must start with $")
	}
	if path == "$" {
		return nil, nil
	}
	segments := make([]jsonPathSegment, 0, 4)
	for i := 1; i < len(path); {
		switch path[i] {
		case '.':
			i++
			start := i
			for i < len(path) && (isIdentChar(path[i])) {
				i++
			}
			if start == i {
				return nil, fmt.Errorf("invalid JSONPath near %q", path)
			}
			segments = append(segments, jsonPathSegment{isKey: true, key: path[start:i]})
		case '[':
			i++
			if i >= len(path) {
				return nil, fmt.Errorf("unterminated JSONPath bracket")
			}
			if path[i] == '\'' {
				i++
				start := i
				for i < len(path) && path[i] != '\'' {
					i++
				}
				if i >= len(path) {
					return nil, fmt.Errorf("unterminated JSONPath quoted key")
				}
				key := path[start:i]
				i++
				if i >= len(path) || path[i] != ']' {
					return nil, fmt.Errorf("unterminated JSONPath quoted key")
				}
				i++
				segments = append(segments, jsonPathSegment{isKey: true, key: key})
				continue
			}
			start := i
			for i < len(path) && path[i] != ']' {
				i++
			}
			if i >= len(path) {
				return nil, fmt.Errorf("unterminated JSONPath index")
			}
			idxRaw := strings.TrimSpace(path[start:i])
			i++
			idx, err := strconv.Atoi(idxRaw)
			if err != nil {
				return nil, fmt.Errorf("invalid JSONPath index %q", idxRaw)
			}
			segments = append(segments, jsonPathSegment{index: idx})
		default:
			return nil, fmt.Errorf("unsupported JSONPath token %q", string(path[i]))
		}
	}
	return segments, nil
}

func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
}
