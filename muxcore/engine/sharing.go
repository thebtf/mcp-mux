package engine

import (
	"context"
	"fmt"
	"maps"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
)

func (e *MuxEngine) resolvePartition(ctx context.Context, cwd string, env map[string]string) (partition *control.PartitionAssertion, err error) {
	if !e.cfg.ModernSharing || e.cfg.PartitionResolver == nil {
		return nil, nil
	}
	defer func() {
		if recover() != nil {
			partition = nil
			err = fmt.Errorf("engine: partition resolver panic")
		}
	}()
	assertion, resolveErr := e.cfg.PartitionResolver(ctx, muxcore.ProjectContext{
		ID: muxcore.ProjectContextID(cwd), Cwd: cwd, Env: maps.Clone(env),
	})
	if resolveErr != nil {
		return nil, fmt.Errorf("engine: partition resolver failed")
	}
	if assertion.Known != (assertion.Key != "") {
		return nil, fmt.Errorf("engine: invalid partition assertion")
	}
	return &assertion, nil
}
