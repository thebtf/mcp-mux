package era

import (
	"errors"
	"fmt"
	"testing"
)

func TestProtocolEraWireContract(t *testing.T) {
	if got := ProtocolEra(0); got != EraLegacy {
		t.Fatalf("zero ProtocolEra = %v, want EraLegacy", got)
	}
	if got := ProtocolPolicy(0); got != PolicyLegacyOnly {
		t.Fatalf("zero ProtocolPolicy = %v, want PolicyLegacyOnly", got)
	}

	for _, tt := range []struct {
		name string
		wire string
		want ProtocolEra
	}{
		{name: "empty is legacy", wire: "", want: EraLegacy},
		{name: "exact modern version", wire: "2026-07-28", want: EraModern20260728},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProtocolEra(tt.wire)
			if err != nil {
				t.Fatalf("ParseProtocolEra(%q) error = %v", tt.wire, err)
			}
			if got != tt.want {
				t.Fatalf("ParseProtocolEra(%q) = %v, want %v", tt.wire, got, tt.want)
			}
		})
	}

	for _, wire := range []string{"legacy", "2026-07-28 ", " 2026-07-28", "2025-03-26", "2026-07-29"} {
		if got, err := ParseProtocolEra(wire); err == nil {
			t.Errorf("ParseProtocolEra(%q) = %v, want rejection", wire, got)
		}
	}

	for _, tt := range []struct {
		era  ProtocolEra
		want string
	}{
		{era: EraLegacy, want: ""},
		{era: EraModern20260728, want: "2026-07-28"},
	} {
		got, err := tt.era.Wire()
		if err != nil {
			t.Fatalf("ProtocolEra(%v).Wire() error = %v", tt.era, err)
		}
		if got != tt.want {
			t.Errorf("ProtocolEra(%v).Wire() = %q, want %q", tt.era, got, tt.want)
		}
	}

	if got, err := ProtocolEra(255).Wire(); err == nil {
		t.Errorf("ProtocolEra(255).Wire() = %q, want rejection", got)
	}
}

func TestOpeningFrameOwnsAndTransfersRawFrame(t *testing.T) {
	raw := []byte("{\"jsonrpc\":\"2.0\",\"method\":\"tools/list\"}\n")
	frame := NewOpeningFrame(raw)
	if frame == nil {
		t.Fatal("NewOpeningFrame returned nil")
	}
	if len(frame.raw) == 0 {
		t.Fatal("NewOpeningFrame did not retain raw frame")
	}
	ownedFirstByte := &frame.raw[0]

	raw[0] = '!'
	got, ok := frame.Take()
	if !ok {
		t.Fatal("first Take() = unavailable, want owned frame")
	}
	if want := "{\"jsonrpc\":\"2.0\",\"method\":\"tools/list\"}\n"; string(got) != want {
		t.Fatalf("Take() = %q, want %q after caller mutation", got, want)
	}
	if &got[0] != ownedFirstByte {
		t.Fatal("Take() copied owned frame instead of transferring it")
	}

	again, ok := frame.Take()
	if ok || len(again) != 0 {
		t.Fatalf("second Take() = (%q, %v), want (empty, false)", again, ok)
	}
}

func TestAdmissionErrorCategoriesAreRedactedAndWrappable(t *testing.T) {
	for _, kind := range []AdmissionErrorKind{
		AdmissionMalformedFrame,
		AdmissionInvalidModernParams,
		AdmissionUnsupportedModernVersion,
		AdmissionConflictingEraSignals,
		AdmissionControlEraMismatch,
		AdmissionUnsafeLifecycleBoundary,
		AdmissionContainedUpstreamRequest,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			admission := NewAdmissionError(kind)
			if got := admission.Kind(); got != kind {
				t.Fatalf("AdmissionError.Kind() = %v, want %v", got, kind)
			}
			if got, want := admission.Error(), "admission: "+kind.String(); got != want {
				t.Fatalf("AdmissionError.Error() = %q, want redacted category %q", got, want)
			}

			wrapped := fmt.Errorf("ingress: %w", admission)
			if !errors.Is(wrapped, kind) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", wrapped, kind)
			}
			if !errors.Is(wrapped, NewAdmissionError(kind)) {
				t.Fatalf("errors.Is(%v, matching AdmissionError) = false, want true", wrapped)
			}
			var recovered *AdmissionError
			if !errors.As(wrapped, &recovered) {
				t.Fatalf("errors.As(%v, *AdmissionError) = false, want true", wrapped)
			}
			if got := recovered.Kind(); got != kind {
				t.Fatalf("recovered AdmissionError.Kind() = %v, want %v", got, kind)
			}
		})
	}
}
