package pi

import "fmt"

// validateToolConfig is the shared validator for the registered tool set and the
// active subset, used by NewAgent, SetTools, and SetActiveTools. Registered tool
// names must be unique; a nil active set means "all registered tools active",
// while a non-nil active set must reference only registered names without
// duplicates.
func validateToolConfig(tools []RegisteredTool, activeNames []string) error {
	registered := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		name := t.Name()
		if _, dup := registered[name]; dup {
			return fmt.Errorf("tool %q: %w", name, ErrDuplicateToolName)
		}
		registered[name] = struct{}{}
	}
	if activeNames == nil {
		return nil
	}
	active := make(map[string]struct{}, len(activeNames))
	for _, name := range activeNames {
		if _, ok := registered[name]; !ok {
			return fmt.Errorf("active tool %q: %w", name, ErrUnknownActiveTool)
		}
		if _, dup := active[name]; dup {
			return fmt.Errorf("active tool %q: %w", name, ErrDuplicateToolName)
		}
		active[name] = struct{}{}
	}
	return nil
}

// filterActiveTools returns the subset of tools that are active. A nil active set
// means all tools are active. The result is always a fresh slice so callers may
// retain it without aliasing the inputs.
func filterActiveTools(tools []RegisteredTool, activeNames []string) []RegisteredTool {
	if activeNames == nil {
		out := make([]RegisteredTool, len(tools))
		copy(out, tools)
		return out
	}
	active := make(map[string]struct{}, len(activeNames))
	for _, n := range activeNames {
		active[n] = struct{}{}
	}
	var out []RegisteredTool
	for _, t := range tools {
		if _, ok := active[t.Name()]; ok {
			out = append(out, t)
		}
	}
	return out
}

// activeToolNamesFromTranscript returns the active set recorded by the last
// active_tools_change entry in the transcript. The second return is false when no
// such entry exists, meaning all registered tools are active.
func activeToolNamesFromTranscript(msgs []Message) (names []string, ok bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if names, ok := msgs[i].ActiveToolNames(); ok {
			return names, true
		}
	}
	return nil, false
}

func toolNames(tools []RegisteredTool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	return names
}

// sameStringSet reports whether a and b contain the same names, ignoring order
// and duplicates.
func sameStringSet(a, b []string) bool {
	sa := make(map[string]struct{}, len(a))
	for _, s := range a {
		sa[s] = struct{}{}
	}
	sb := make(map[string]struct{}, len(b))
	for _, s := range b {
		sb[s] = struct{}{}
	}
	if len(sa) != len(sb) {
		return false
	}
	for s := range sa {
		if _, ok := sb[s]; !ok {
			return false
		}
	}
	return true
}
