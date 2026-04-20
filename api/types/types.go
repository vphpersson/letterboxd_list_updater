package types

type UpdateList struct {
	List string `json:"list" jsonschema:"list"`
	Data string `json:"data" jsonschema:"data"`
}

// ParsedUpdate is the stored form of UpdateList after the body processor
// has parsed Data into ImportEntry rows and validated the Review format.
type ParsedUpdate struct {
	List    string
	Entries []*ImportEntry
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
