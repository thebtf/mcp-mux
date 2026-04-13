package remap

import (
	"encoding/json"
	"testing"
)

func TestRemapNumber(t *testing.T) {
	original := json.RawMessage(`1`)
	remapped := Remap(3, original)

	want := `"s3:n:1"`
	if string(remapped) != want {
		t.Errorf("Remap(3, 1) = %s, want %s", string(remapped), want)
	}
}

func TestRemapString(t *testing.T) {
	original := json.RawMessage(`"abc"`)
	remapped := Remap(5, original)

	want := `"s5:s:abc"`
	if string(remapped) != want {
		t.Errorf("Remap(5, \"abc\") = %s, want %s", string(remapped), want)
	}
}

func TestRemapNull(t *testing.T) {
	original := json.RawMessage(`null`)
	remapped := Remap(1, original)

	want := `"s1:null"`
	if string(remapped) != want {
		t.Errorf("Remap(1, null) = %s, want %s", string(remapped), want)
	}
}

func TestRemapNilID(t *testing.T) {
	remapped := Remap(2, nil)

	want := `"s2:null"`
	if string(remapped) != want {
		t.Errorf("Remap(2, nil) = %s, want %s", string(remapped), want)
	}
}

func TestRemapLargeNumber(t *testing.T) {
	original := json.RawMessage(`42`)
	remapped := Remap(100, original)

	want := `"s100:n:42"`
	if string(remapped) != want {
		t.Errorf("Remap(100, 42) = %s, want %s", string(remapped), want)
	}
}

func TestDeremapNumber(t *testing.T) {
	remapped := json.RawMessage(`"s3:n:1"`)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("Deremap(%s) error: %v", string(remapped), err)
	}
	if result.SessionID != 3 {
		t.Errorf("SessionID = %d, want 3", result.SessionID)
	}
	if string(result.OriginalID) != "1" {
		t.Errorf("OriginalID = %s, want 1", string(result.OriginalID))
	}
}

func TestDeremapString(t *testing.T) {
	remapped := json.RawMessage(`"s5:s:abc"`)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("Deremap(%s) error: %v", string(remapped), err)
	}
	if result.SessionID != 5 {
		t.Errorf("SessionID = %d, want 5", result.SessionID)
	}
	if string(result.OriginalID) != `"abc"` {
		t.Errorf("OriginalID = %s, want \"abc\"", string(result.OriginalID))
	}
}

func TestDeremapNull(t *testing.T) {
	remapped := json.RawMessage(`"s1:null"`)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("Deremap(%s) error: %v", string(remapped), err)
	}
	if result.SessionID != 1 {
		t.Errorf("SessionID = %d, want 1", result.SessionID)
	}
	if string(result.OriginalID) != "null" {
		t.Errorf("OriginalID = %s, want null", string(result.OriginalID))
	}
}

func TestRoundtripNumber(t *testing.T) {
	original := json.RawMessage(`42`)
	remapped := Remap(7, original)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("roundtrip error: %v", err)
	}
	if result.SessionID != 7 {
		t.Errorf("SessionID = %d, want 7", result.SessionID)
	}
	if string(result.OriginalID) != "42" {
		t.Errorf("OriginalID = %s, want 42", string(result.OriginalID))
	}
}

func TestRoundtripString(t *testing.T) {
	original := json.RawMessage(`"hello-world"`)
	remapped := Remap(2, original)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("roundtrip error: %v", err)
	}
	if result.SessionID != 2 {
		t.Errorf("SessionID = %d, want 2", result.SessionID)
	}
	if string(result.OriginalID) != `"hello-world"` {
		t.Errorf("OriginalID = %s, want \"hello-world\"", string(result.OriginalID))
	}
}

func TestRoundtripNull(t *testing.T) {
	original := json.RawMessage(`null`)
	remapped := Remap(9, original)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("roundtrip error: %v", err)
	}
	if result.SessionID != 9 {
		t.Errorf("SessionID = %d, want 9", result.SessionID)
	}
	if string(result.OriginalID) != "null" {
		t.Errorf("OriginalID = %s, want null", string(result.OriginalID))
	}
}

func TestDeremapInvalidFormat(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`"no-prefix"`),
		json.RawMessage(`"sNaN:n:1"`),
		json.RawMessage(`123`), // not a string
		json.RawMessage(`"s1"`),
	}

	for _, c := range cases {
		_, err := Deremap(c)
		if err == nil {
			t.Errorf("Deremap(%s) expected error, got nil", string(c))
		}
	}
}

func TestIsRemapped(t *testing.T) {
	tests := []struct {
		id   json.RawMessage
		want bool
	}{
		{json.RawMessage(`"s1:n:1"`), true},
		{json.RawMessage(`"s5:s:abc"`), true},
		{json.RawMessage(`"s1:null"`), true},
		{json.RawMessage(`1`), false},
		{json.RawMessage(`"abc"`), false},
		{json.RawMessage(`null`), false},
	}

	for _, tt := range tests {
		got := IsRemapped(tt.id)
		if got != tt.want {
			t.Errorf("IsRemapped(%s) = %v, want %v", string(tt.id), got, tt.want)
		}
	}
}

func TestRemapZeroSession(t *testing.T) {
	original := json.RawMessage(`1`)
	remapped := Remap(0, original)
	result, err := Deremap(remapped)
	if err != nil {
		t.Fatalf("roundtrip error: %v", err)
	}
	if result.SessionID != 0 {
		t.Errorf("SessionID = %d, want 0", result.SessionID)
	}
}
