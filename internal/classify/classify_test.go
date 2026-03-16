package classify

import "testing"

func TestClassifyTools(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantMode    SharingMode
		wantMatched int // number of matched tools
	}{
		{
			name: "playwright tools - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"browser_navigate"},
				{"name":"browser_click"},
				{"name":"browser_snapshot"},
				{"name":"browser_take_screenshot"},
				{"name":"browser_type"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 5,
		},
		{
			name: "tavily - shared",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"tavily_search"},
				{"name":"tavily_extract"}
			]}}`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "engram - shared",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"search"},
				{"name":"store_memory"},
				{"name":"recall_memory"},
				{"name":"decisions"}
			]}}`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "serena - shared",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"find_symbol"},
				{"name":"get_symbols_overview"},
				{"name":"replace_symbol_body"},
				{"name":"search_for_pattern"}
			]}}`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "mixed stateless and stateful - isolated wins",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"search"},
				{"name":"browser_navigate"},
				{"name":"store"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 1,
		},
		{
			name: "empty tools - shared",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "invalid json - shared default",
			json: `not json`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "session prefix - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"session_create"},
				{"name":"session_destroy"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "editor prefix - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"editor_open"},
				{"name":"editor_save"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "navigate prefix - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"navigate_to"},
				{"name":"navigate_back"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "page prefix - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"page_goto"},
				{"name":"page_content"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "tab prefix - isolated",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"tab_open"},
				{"name":"tab_close"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "case insensitive matching",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"Browser_Navigate"},
				{"name":"SESSION_Create"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 2,
		},
		{
			name: "no result field - shared default",
			json: `{"jsonrpc":"2.0","id":1}`,
			wantMode:    ModeShared,
			wantMatched: 0,
		},
		{
			name: "desktop-commander - isolated via substring",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"start_process"},
				{"name":"interact_with_process"},
				{"name":"read_file"},
				{"name":"write_file"},
				{"name":"kill_process"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 3,
		},
		{
			name: "pencil - isolated via substring",
			json: `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"open_document"},
				{"name":"batch_design"},
				{"name":"get_editor_state"},
				{"name":"snapshot_layout"},
				{"name":"get_screenshot"}
			]}}`,
			wantMode:    ModeIsolated,
			wantMatched: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, matched := ClassifyTools([]byte(tt.json))
			if mode != tt.wantMode {
				t.Errorf("ClassifyTools() mode = %v, want %v", mode, tt.wantMode)
			}
			if len(matched) != tt.wantMatched {
				t.Errorf("ClassifyTools() matched %d tools, want %d: %v",
					len(matched), tt.wantMatched, matched)
			}
		})
	}
}

func TestIsIsolationPattern(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"browser_navigate", true},
		{"browser_click", true},
		{"Browser_Navigate", true},
		{"BROWSER_CLICK", true},
		{"session_create", true},
		{"editor_open", true},
		{"navigate_to", true},
		{"navigateBack", true},
		{"page_goto", true},
		{"tab_open", true},
		{"start_process", true},         // substring: _process
		{"interact_with_process", true},  // substring: _process
		{"kill_process", true},           // substring: _process
		{"open_document", true},          // substring: _document
		{"get_editor_state", true},       // substring: _editor_
		{"snapshot_layout", true},        // substring: _snapshot
		{"search", false},
		{"store_memory", false},
		{"tavily_search", false},
		{"find_symbol", false},
		{"echo", false},
		{"add", false},
		{"read_file", false},
		{"write_file", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIsolationPattern(tt.name)
			if got != tt.want {
				t.Errorf("isIsolationPattern(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestClassifyCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantMode SharingMode
		wantOK   bool
	}{
		{
			name:     "x-mux shared",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"x-mux":{"sharing":"shared"}}}}`,
			wantMode: ModeShared,
			wantOK:   true,
		},
		{
			name:     "x-mux isolated",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"x-mux":{"sharing":"isolated"}}}}`,
			wantMode: ModeIsolated,
			wantOK:   true,
		},
		{
			name:     "x-mux session-aware",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"x-mux":{"sharing":"session-aware"}}}}`,
			wantMode: ModeSessionAware,
			wantOK:   true,
		},
		{
			name:     "no x-mux capability",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}}}}`,
			wantMode: "",
			wantOK:   false,
		},
		{
			name:     "x-mux in experimental (TypeScript SDK)",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"experimental":{"x-mux":{"sharing":"shared","stateless":true}}}}}`,
			wantMode: ModeShared,
			wantOK:   true,
		},
		{
			name:     "session-aware in experimental",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"experimental":{"x-mux":{"sharing":"session-aware"}}}}}`,
			wantMode: ModeSessionAware,
			wantOK:   true,
		},
		{
			name:     "invalid sharing value",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"sharing":"bogus"}}}}`,
			wantMode: "",
			wantOK:   false,
		},
		{
			name:     "invalid json",
			json:     `not json`,
			wantMode: "",
			wantOK:   false,
		},
		{
			name:     "empty capabilities",
			json:     `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`,
			wantMode: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, ok := ClassifyCapabilities([]byte(tt.json))
			if mode != tt.wantMode {
				t.Errorf("ClassifyCapabilities() mode = %v, want %v", mode, tt.wantMode)
			}
			if ok != tt.wantOK {
				t.Errorf("ClassifyCapabilities() ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestParsePersistent(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "persistent true via x-mux",
			json: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"sharing":"shared","persistent":true}}}}`,
			want: true,
		},
		{
			name: "persistent false via x-mux",
			json: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"sharing":"shared","persistent":false}}}}`,
			want: false,
		},
		{
			name: "persistent missing",
			json: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"sharing":"shared"}}}}`,
			want: false,
		},
		{
			name: "persistent true via experimental",
			json: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"experimental":{"x-mux":{"sharing":"shared","persistent":true}}}}}`,
			want: true,
		},
		{
			name: "no x-mux at all",
			json: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}}}}`,
			want: false,
		},
		{
			name: "invalid json",
			json: `not json`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePersistent([]byte(tt.json))
			if got != tt.want {
				t.Errorf("ParsePersistent() = %v, want %v", got, tt.want)
			}
		})
	}
}
