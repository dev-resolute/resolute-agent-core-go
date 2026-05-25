package compaction

// Opts carries options for a compaction operation.
type Opts struct {
	KeepRecentTokens int
}

// Result carries the outcome of a compaction.
type Result struct {
	Summary      string
	RemovedCount int
}
