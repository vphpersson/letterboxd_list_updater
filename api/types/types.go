package types

type UpdateList struct {
	List string `json:"list" jsonschema:"list"`
	Data string `json:"data" jsonschema:"data"`
	// DryRun walks the import up to, but not including, the request that
	// commits it, so the flow can be checked against the live site without
	// changing the list.
	DryRun bool `json:"dry_run,omitzero" jsonschema:"dry_run,optional"`
}

// ParsedUpdate is the stored form of UpdateList after the body processor
// has parsed Data into ImportEntry rows and validated the Review format.
type ParsedUpdate struct {
	List    string
	Entries []*ImportEntry
	DryRun  bool
}

// UpdateResult is what an update did to the list.
type UpdateResult struct {
	List string `json:"list"`
	// Matched is how many rows Letterboxd resolved to a film.
	Matched int `json:"matched"`
	// Added is how many of those were not on the list already.
	Added  int  `json:"added"`
	DryRun bool `json:"dry_run,omitzero"`
}

// ImportEntry is a single row in the Letterboxd import CSV format.
// At least one of LetterboxdURI, TmdbID, ImdbID, Title must be set.
// Review doubles as the Notes field when importing to a list.
type ImportEntry struct {
	LetterboxdURI string `csv:"LetterboxdURI,omitempty"`
	TmdbID        string `csv:"tmdbID,omitempty"`
	ImdbID        string `csv:"imdbID,omitempty"`
	Title         string `csv:"Title,omitempty"`
	Year          string `csv:"Year,omitempty"`
	Directors     string `csv:"Directors,omitempty"`
	Rating        string `csv:"Rating,omitempty"`
	Rating10      string `csv:"Rating10,omitempty"`
	WatchedDate   string `csv:"WatchedDate,omitempty"`
	Rewatch       string `csv:"Rewatch,omitempty"`
	Tags          string `csv:"Tags,omitempty"`
	Review        string `csv:"Review,omitempty"`
}
