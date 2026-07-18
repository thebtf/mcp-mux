package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalJSONNumber(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"0":          "0",
		"-0":         "0",
		"1":          "1e0",
		"1.0":        "1e0",
		"1e0":        "1e0",
		"1000":       "1e3",
		"1e3":        "1e3",
		"0.00100":    "1e-3",
		"-12.3400e2": "-1234e0",
		"123e-999":   "123e-999",
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := canonicalJSONNumber(input)
			if err != nil {
				t.Fatalf("canonicalJSONNumber(%q): %v", input, err)
			}
			if got != want {
				t.Fatalf("canonicalJSONNumber(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestCanonicalJSONNumberRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "+1", "01", "1.", ".1", "1e", "--1", "null", "true"} {
		if _, err := canonicalJSONNumber(input); err == nil {
			t.Errorf("canonicalJSONNumber(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseScalarUsesSemanticStringAndRawBytes(t *testing.T) {
	t.Parallel()

	plain, err := parseScalar(json.RawMessage(`"a"`))
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := parseScalar(json.RawMessage(`"\u0061"`))
	if err != nil {
		t.Fatal(err)
	}
	if plain.key != escaped.key {
		t.Fatalf("escaped string keys differ: %#v != %#v", plain.key, escaped.key)
	}
	if string(escaped.raw) != `"\u0061"` {
		t.Fatalf("raw escaped ID = %s", escaped.raw)
	}

	one, err := parseScalar(json.RawMessage(`1`))
	if err != nil {
		t.Fatal(err)
	}
	oneExponent, err := parseScalar(json.RawMessage(`1e0`))
	if err != nil {
		t.Fatal(err)
	}
	if one.key != oneExponent.key {
		t.Fatalf("numeric keys differ: %#v != %#v", one.key, oneExponent.key)
	}
}

func TestParseScalarRejectsOversizedCorrelationValue(t *testing.T) {
	value := `"` + strings.Repeat("x", maxCorrelationValueBytes) + `"`
	if _, err := parseScalar(json.RawMessage(value)); err == nil {
		t.Fatal("oversized scalar was accepted")
	}
}
