package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

type scalarKind uint8

const (
	scalarString scalarKind = iota + 1
	scalarNumber
)

type scalarKey struct {
	kind  scalarKind
	value string
}

type scalarValue struct {
	key scalarKey
	raw json.RawMessage
}

func parseScalar(raw json.RawMessage) (scalarValue, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return scalarValue{}, fmt.Errorf("empty scalar")
	}
	if len(trimmed) > maxCorrelationValueBytes {
		return scalarValue{}, fmt.Errorf("scalar exceeds correlation limit")
	}

	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return scalarValue{}, fmt.Errorf("string scalar: %w", err)
		}
		return scalarValue{
			key: scalarKey{kind: scalarString, value: value},
			raw: cloneRaw(trimmed),
		}, nil
	}

	canonical, err := canonicalJSONNumber(string(trimmed))
	if err != nil {
		return scalarValue{}, err
	}
	return scalarValue{
		key: scalarKey{kind: scalarNumber, value: canonical},
		raw: cloneRaw(trimmed),
	}, nil
}

func parseString(raw json.RawMessage, field string) (string, error) {
	var value string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &value); err != nil {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return value, nil
}

// canonicalJSONNumber returns an exact decimal value key without converting
// through binary floating point. Equivalent JSON forms such as 1, 1.0 and 1e0
// therefore compare equal.
func canonicalJSONNumber(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("invalid JSON number")
	}

	i := 0
	negative := false
	if value[i] == '-' {
		negative = true
		i++
		if i == len(value) {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
	}

	integerStart := i
	switch {
	case value[i] == '0':
		i++
		if i < len(value) && value[i] >= '0' && value[i] <= '9' {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
	case value[i] >= '1' && value[i] <= '9':
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
	default:
		return "", fmt.Errorf("invalid JSON number %q", value)
	}
	integerPart := value[integerStart:i]

	fractionPart := ""
	if i < len(value) && value[i] == '.' {
		i++
		fractionStart := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == fractionStart {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
		fractionPart = value[fractionStart:i]
	}

	exponent := new(big.Int)
	if i < len(value) && (value[i] == 'e' || value[i] == 'E') {
		i++
		exponentNegative := false
		if i < len(value) && (value[i] == '+' || value[i] == '-') {
			exponentNegative = value[i] == '-'
			i++
		}
		exponentStart := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == exponentStart {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
		if _, ok := exponent.SetString(value[exponentStart:i], 10); !ok {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
		if exponentNegative {
			exponent.Neg(exponent)
		}
	}
	if i != len(value) {
		return "", fmt.Errorf("invalid JSON number %q", value)
	}

	digits := strings.TrimLeft(integerPart+fractionPart, "0")
	if digits == "" {
		return "0", nil
	}

	scale := new(big.Int).Set(exponent)
	scale.Sub(scale, big.NewInt(int64(len(fractionPart))))
	trailing := len(digits) - len(strings.TrimRight(digits, "0"))
	if trailing > 0 {
		digits = digits[:len(digits)-trailing]
		scale.Add(scale, big.NewInt(int64(trailing)))
	}

	prefix := ""
	if negative {
		prefix = "-"
	}
	return prefix + digits + "e" + scale.String(), nil
}

func cloneRaw(raw []byte) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}
