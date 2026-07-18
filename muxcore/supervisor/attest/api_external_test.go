package attest_test

import (
	"context"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
)

type admission interface {
	Verified() bool
	Close() error
}

var (
	_ admission                                                          = (*attest.Parent)(nil)
	_ func(context.Context, attest.ParentConfig) (*attest.Parent, error) = attest.StartParent
	_ func(context.Context, attest.VerifyConfig) error                   = attest.VerifyParent
	_                                                                    = attest.Advertisement{Version: "2", ParentPID: 1, Endpoint: "endpoint"}
	_                                                                    = attest.ParentConfig{Lifetime: time.Second, IOTimeout: time.Second}
	_                                                                    = attest.VerifyConfig{
		Advertisement:   attest.Advertisement{},
		DirectParentPID: 1,
		IOTimeout:       time.Second,
	}
)
