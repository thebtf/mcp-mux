package classify

import "testing"

func TestParseProgressInterval_DirectXMux(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"x-mux":{"progressInterval":10}}}}`)
	if got := ParseProgressInterval(in); got != 10 {
		t.Errorf("want 10, got %d", got)
	}
}

func TestParseProgressInterval_Experimental(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"experimental":{"x-mux":{"progressInterval":30}}}}}`)
	if got := ParseProgressInterval(in); got != 30 {
		t.Errorf("want 30, got %d", got)
	}
}

func TestParseProgressInterval_Absent(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"tools":{}}}}`)
	if got := ParseProgressInterval(in); got != 0 {
		t.Errorf("want 0 when absent, got %d", got)
	}
}

func TestParseProgressInterval_Negative(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"x-mux":{"progressInterval":-1}}}}`)
	if got := ParseProgressInterval(in); got != 0 {
		t.Errorf("want 0 for negative, got %d", got)
	}
}

func TestParseProgressInterval_Cap(t *testing.T) {
	// 120s → clamped to 60s
	in := []byte(`{"result":{"capabilities":{"x-mux":{"progressInterval":120}}}}`)
	if got := ParseProgressInterval(in); got != 60 {
		t.Errorf("want clamped to 60, got %d", got)
	}
}

func TestParseProgressInterval_InvalidJSON(t *testing.T) {
	if got := ParseProgressInterval([]byte("garbage")); got != 0 {
		t.Errorf("want 0 on parse error, got %d", got)
	}
}
