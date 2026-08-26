package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Metadata budget limits. They keep a misbehaving caller from turning an
// observability record into an unbounded sink while leaving generous room for
// application correlation data.
const (
	// MaxMetadataEntries caps the number of keys in one Metadata value.
	MaxMetadataEntries = 32
	// MaxMetadataBytes caps the combined encoded size of all keys and values.
	MaxMetadataBytes = 16 << 10
	// MaxMetadataDepth caps the JSON nesting depth of any single value.
	MaxMetadataDepth = 8
	// maxMetadataKeyLength caps the length of one namespaced key.
	maxMetadataKeyLength = 128
	// reservedMetadataNamespace is reserved for runtime-owned keys so
	// application metadata can never shadow future runtime annotations.
	reservedMetadataNamespace = "lebro"
)

// Validate enforces the durable-metadata contract: namespaced keys, valid
// JSON values, and bounded size and depth. Nil and empty Metadata are valid.
func (m Metadata) Validate() error {
	if len(m) == 0 {
		return nil
	}
	if len(m) > MaxMetadataEntries {
		return fmt.Errorf("lebro: metadata has %d entries, limit is %d", len(m), MaxMetadataEntries)
	}
	total := 0
	for key, value := range m {
		if err := validateMetadataKey(key); err != nil {
			return err
		}
		if len(value) == 0 {
			return fmt.Errorf("lebro: metadata key %q has an empty value", key)
		}
		if !json.Valid(value) {
			return fmt.Errorf("lebro: metadata key %q value must be valid JSON", key)
		}
		if depth := jsonDepth(value); depth > MaxMetadataDepth {
			return fmt.Errorf("lebro: metadata key %q value depth %d exceeds limit %d", key, depth, MaxMetadataDepth)
		}
		total += len(key) + len(value)
		if total > MaxMetadataBytes {
			return fmt.Errorf("lebro: metadata exceeds %d bytes", MaxMetadataBytes)
		}
	}
	return nil
}

// Clone returns a caller-owned deep copy of the metadata.
func (m Metadata) Clone() Metadata {
	if len(m) == 0 {
		return nil
	}
	cloned := make(Metadata, len(m))
	for key, value := range m {
		cloned[key] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

func validateMetadataKey(key string) error {
	dot := strings.IndexByte(key, '.')
	if dot <= 0 || dot == len(key)-1 {
		return fmt.Errorf("lebro: metadata key %q must be namespaced like \"app.customer_id\"", key)
	}
	namespace, name := key[:dot], key[dot+1:]
	if !isMetadataSegment(namespace) {
		return fmt.Errorf("lebro: metadata namespace %q must contain only letters, digits, underscores, and hyphens", namespace)
	}
	if namespace == reservedMetadataNamespace {
		return fmt.Errorf("lebro: metadata namespace %q is reserved", reservedMetadataNamespace)
	}
	if !isMetadataSegment(name) {
		return fmt.Errorf("lebro: metadata key suffix %q must contain only letters, digits, underscores, dots, and hyphens", name)
	}
	if len(key) > maxMetadataKeyLength {
		return fmt.Errorf("lebro: metadata key %q exceeds %d bytes", key, maxMetadataKeyLength)
	}
	return nil
}

func isMetadataSegment(segment string) bool {
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// jsonDepth returns the nesting depth of an already JSON-valid value. Scalars
// have depth 1; each nested array or object level adds one.
func jsonDepth(value []byte) int {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		// Callers validate JSON before measuring depth.
		return 1
	}
	return jsonValueDepth(decoded)
}

func jsonValueDepth(value any) int {
	switch typed := value.(type) {
	case []any:
		depth := 1
		for _, item := range typed {
			if inner := jsonValueDepth(item) + 1; inner > depth {
				depth = inner
			}
		}
		return depth
	case map[string]any:
		depth := 1
		for _, item := range typed {
			if inner := jsonValueDepth(item) + 1; inner > depth {
				depth = inner
			}
		}
		return depth
	default:
		return 1
	}
}
