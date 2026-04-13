package jsonrpc

import (
	"encoding/json"
	"fmt"
)

// InjectMeta adds a key-value pair to the _meta object inside params.
// If params or _meta don't exist, they are created.
// Existing _meta fields are preserved — only the specified key is added/overwritten.
// Value can be any JSON-serializable type (string, map, slice, etc.).
func InjectMeta(raw []byte, key string, value any) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("inject meta: parse message: %w", err)
	}

	// Get or create params
	var params map[string]json.RawMessage
	if paramsRaw, ok := obj["params"]; ok {
		if err := json.Unmarshal(paramsRaw, &params); err != nil {
			// params exists but isn't an object — can't inject
			return nil, fmt.Errorf("inject meta: parse params: %w", err)
		}
	} else {
		params = make(map[string]json.RawMessage)
	}

	// Get or create _meta
	var meta map[string]json.RawMessage
	if metaRaw, ok := params["_meta"]; ok {
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			meta = make(map[string]json.RawMessage)
		}
	} else {
		meta = make(map[string]json.RawMessage)
	}

	// Set the key
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("inject meta: marshal value: %w", err)
	}
	meta[key] = valueJSON

	// Re-serialize
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("inject meta: marshal _meta: %w", err)
	}
	params["_meta"] = metaJSON

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("inject meta: marshal params: %w", err)
	}
	obj["params"] = paramsJSON

	result, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("inject meta: marshal message: %w", err)
	}

	return result, nil
}
