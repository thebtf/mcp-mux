package attest

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

type pastDeadlineContext struct{}

func (pastDeadlineContext) Deadline() (time.Time, bool) { return time.Unix(0, 1), true }
func (pastDeadlineContext) Done() <-chan struct{}       { return nil }
func (pastDeadlineContext) Err() error                  { return nil }
func (pastDeadlineContext) Value(any) any               { return nil }

func TestVerifyParentPastDeadlineFailsClosed(t *testing.T) {
	err := VerifyParent(pastDeadlineContext{}, VerifyConfig{
		Advertisement:   Advertisement{Version: VersionV2, ParentPID: os.Getpid(), Endpoint: "unused"},
		DirectParentPID: os.Getpid(),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("VerifyParent error = %v, want context deadline exceeded", err)
	}
}

func TestParentChildDirectPairAttests(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	advertisement := parent.Advertisement()
	if advertisement.Version != VersionV2 || advertisement.ParentPID != os.Getpid() || advertisement.Endpoint == "" {
		t.Fatalf("advertisement = %#v", advertisement)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   advertisement,
		DirectParentPID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parent.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent receipt did not finish")
	}
	if !parent.Verified() {
		t.Fatal("parent did not record exact child receipt")
	}
}

func TestParentAcceptsChildThatConnectsBeforeBind(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	result := make(chan error, 1)
	go func() {
		result <- VerifyParent(context.Background(), VerifyConfig{
			Advertisement:   parent.Advertisement(),
			DirectParentPID: os.Getpid(),
			IOTimeout:       500 * time.Millisecond,
		})
	}()
	time.Sleep(25 * time.Millisecond)
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("pre-bind connection failed: %v", err)
	}
	select {
	case <-parent.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("pre-bind parent receipt did not finish")
	}
	if !parent.Verified() {
		t.Fatal("pre-bind connection did not produce receipt")
	}
}

func TestParentRejectsWrongChildPIDAndContinues(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid() + 1000); err != nil {
		t.Fatal(err)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid(),
		IOTimeout:       100 * time.Millisecond,
	}); err == nil {
		t.Fatal("wrong child PID unexpectedly attested")
	}
	if parent.Verified() {
		t.Fatal("wrong child PID produced a receipt")
	}
	select {
	case <-parent.Done():
		t.Fatal("wrong child PID closed the reusable receipt before expiry")
	default:
	}
}

func TestParentRejectsMalformedExchangeThenAcceptsValidChild(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.DialTimeout(parent.Advertisement().Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "not-attestation\n")
	_ = conn.Close()
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid(),
	}); err != nil {
		t.Fatalf("valid child after malformed peer: %v", err)
	}
	select {
	case <-parent.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("valid-child parent receipt did not finish")
	}
	if !parent.Verified() {
		t.Fatal("valid child after malformed peer did not verify")
	}
}

func TestVerifyParentRejectsForwardedAdvertisement(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid() + 1,
	}); err == nil {
		t.Fatal("forwarded parent advertisement unexpectedly verified")
	}
}

func TestVerifyParentRejectsWrongServerPID(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	advertisement := parent.Advertisement()
	advertisement.ParentPID++
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   advertisement,
		DirectParentPID: advertisement.ParentPID,
	}); err == nil {
		t.Fatal("endpoint server PID mismatch unexpectedly verified")
	}
	if parent.Verified() {
		t.Fatal("server PID mismatch produced a parent receipt")
	}
}

func TestParentExpiresAndCleansEndpoint(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := parent.Advertisement().Endpoint
	select {
	case <-parent.Done():
	case <-time.After(time.Second):
		t.Fatal("parent did not expire")
	}
	if parent.Verified() {
		t.Fatal("expired parent unexpectedly verified")
	}
	if conn, err := ipc.DialTimeout(endpoint, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("expired endpoint remained connectable")
	}
}

func TestParentCannotVerifyAfterLifetimeThroughAcceptedConnection(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{
		Lifetime:  40 * time.Millisecond,
		IOTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.DialTimeout(parent.Advertisement().Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, attestationRequest[:len(attestationRequest)-1]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parent.Done():
	case <-time.After(time.Second):
		t.Fatal("parent did not expire with an accepted connection")
	}
	_, _ = io.WriteString(conn, attestationRequest[len(attestationRequest)-1:])
	time.Sleep(25 * time.Millisecond)
	if parent.Verified() {
		t.Fatal("accepted connection produced a receipt after expiry")
	}
}

func TestBindChildPIDAfterCloseIsRejected(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.BindChildPID(os.Getpid()); err == nil {
		t.Fatal("closed parent accepted a child PID")
	}
}
