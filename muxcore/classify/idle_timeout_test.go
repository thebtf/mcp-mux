package classify

import "testing"

func TestParseIdleTimeout_DirectXMux(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"x-mux":{"idleTimeout":600}}}}`)
	if got := ParseIdleTimeout(in); got != 600 {
		t.Errorf("want 600, got %d", got)
	}
}

func TestParseIdleTimeout_Experimental(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"experimental":{"x-mux":{"idleTimeout":1800}}}}}`)
	if got := ParseIdleTimeout(in); got != 1800 {
		t.Errorf("want 1800, got %d", got)
	}
}

func TestParseIdleTimeout_Absent(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"tools":{}}}}`)
	if got := ParseIdleTimeout(in); got != 0 {
		t.Errorf("want 0 when absent, got %d", got)
	}
}

func TestParseIdleTimeout_Negative(t *testing.T) {
	in := []byte(`{"result":{"capabilities":{"x-mux":{"idleTimeout":-1}}}}`)
	if got := ParseIdleTimeout(in); got != 0 {
		t.Errorf("want 0 for negative, got %d", got)
	}
}

func TestParseIdleTimeout_Cap(t *testing.T) {
	// 48h → clamped to 24h = 86400
	in := []byte(`{"result":{"capabilities":{"x-mux":{"idleTimeout":172800}}}}`)
	if got := ParseIdleTimeout(in); got != 86400 {
		t.Errorf("want clamped to 86400, got %d", got)
	}
}

func TestParseIdleTimeout_InvalidJSON(t *testing.T) {
	if got := ParseIdleTimeout([]byte("garbage")); got != 0 {
		t.Errorf("want 0 on parse error, got %d", got)
	}
}
