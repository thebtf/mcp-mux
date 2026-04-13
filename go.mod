module github.com/thebtf/mcp-mux

go 1.25.4

require github.com/thebtf/mcp-mux/muxcore v0.0.0

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/thejerf/suture/v4 v4.0.6 // indirect
	golang.org/x/sys v0.43.0 // indirect
)

replace github.com/thebtf/mcp-mux/muxcore => ./muxcore
