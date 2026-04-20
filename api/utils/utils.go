package utils

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

var csvColumns = []struct {
	header string
	get    func(*types.ImportEntry) string
}{
	{"LetterboxdURI", func(e *types.ImportEntry) string { return e.LetterboxdURI }},
	{"tmdbID", func(e *types.ImportEntry) string { return e.TmdbID }},
	{"imdbID", func(e *types.ImportEntry) string { return e.ImdbID }},
	{"Title", func(e *types.ImportEntry) string { return e.Title }},
	{"Year", func(e *types.ImportEntry) string { return e.Year }},
	{"Directors", func(e *types.ImportEntry) string { return e.Directors }},
	{"Rating", func(e *types.ImportEntry) string { return e.Rating }},
	{"Rating10", func(e *types.ImportEntry) string { return e.Rating10 }},
	{"WatchedDate", func(e *types.ImportEntry) string { return e.WatchedDate }},
	{"Rewatch", func(e *types.ImportEntry) string { return e.Rewatch }},
	{"Tags", func(e *types.ImportEntry) string { return e.Tags }},
	{"Review", func(e *types.ImportEntry) string { return e.Review }},
}

// ImportEntriesToCSV serializes entries into the Letterboxd import CSV
// format: comma-delimited with no space after commas, fields containing
// commas/quotes/newlines wrapped in double quotes, internal quotes and
// backslashes escaped with a leading backslash. Only columns that carry
// at least one non-empty value are emitted.
func ImportEntriesToCSV(entries []*types.ImportEntry) []byte {
	usedCols := make([]int, 0, len(csvColumns))
	for i := range csvColumns {
		for _, e := range entries {
			if e == nil {
				continue
			}
			if csvColumns[i].get(e) != "" {
				usedCols = append(usedCols, i)
				break
			}
		}
	}

	var buf bytes.Buffer
	for i, col := range usedCols {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(csvColumns[col].header)
	}
	buf.WriteByte('\n')

	for _, e := range entries {
		if e == nil {
			continue
		}
		for i, col := range usedCols {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(encodeField(csvColumns[col].get(e)))
		}
		buf.WriteByte('\n')
	}

	return buf.Bytes()
}

// ParseImportCSV parses a Letterboxd-format CSV: comma-delimited with no
// space after commas, fields optionally wrapped in double quotes, and inside
// quoted fields `\"` escapes a quote and `\\` escapes a backslash (not
// RFC-4180's doubled-quote style). The header row maps columns to
// ImportEntry fields. The `url` header is accepted as an alias for
// `LetterboxdURI` per the documented backwards-compatibility rule.
func ParseImportCSV(data []byte) ([]*types.ImportEntry, error) {
	records, err := parseCSVRecords(data)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}

	colByName := make(map[string]int, len(records[0]))
	for i, name := range records[0] {
		colByName[name] = i
	}
	col := func(name string) int {
		if i, ok := colByName[name]; ok {
			return i
		}
		return -1
	}

	uriCol := col("LetterboxdURI")
	if uriCol < 0 {
		uriCol = col("url")
	}
	tmdbCol := col("tmdbID")
	imdbCol := col("imdbID")
	titleCol := col("Title")
	yearCol := col("Year")
	directorsCol := col("Directors")
	ratingCol := col("Rating")
	rating10Col := col("Rating10")
	watchedDateCol := col("WatchedDate")
	rewatchCol := col("Rewatch")
	tagsCol := col("Tags")
	reviewCol := col("Review")

	get := func(row []string, c int) string {
		if c < 0 || c >= len(row) {
			return ""
		}
		return row[c]
	}

	entries := make([]*types.ImportEntry, 0, len(records)-1)
	for _, row := range records[1:] {
		entries = append(entries, &types.ImportEntry{
			LetterboxdURI: get(row, uriCol),
			TmdbID:        get(row, tmdbCol),
			ImdbID:        get(row, imdbCol),
			Title:         get(row, titleCol),
			Year:          get(row, yearCol),
			Directors:     get(row, directorsCol),
			Rating:        get(row, ratingCol),
			Rating10:      get(row, rating10Col),
			WatchedDate:   get(row, watchedDateCol),
			Rewatch:       get(row, rewatchCol),
			Tags:          get(row, tagsCol),
			Review:        get(row, reviewCol),
		})
	}
	return entries, nil
}

func parseCSVRecords(data []byte) ([][]string, error) {
	var (
		records [][]string
		row     []string
		field   strings.Builder
	)

	inQuotes := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if escaped {
			field.WriteByte(c)
			escaped = false
			continue
		}
		if inQuotes {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inQuotes = false
			default:
				field.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"':
			if field.Len() > 0 {
				return nil, fmt.Errorf("stray quote at byte %d", i)
			}
			inQuotes = true
		case ',':
			row = append(row, field.String())
			field.Reset()
		case '\r':
			// swallowed; \n handles row termination
		case '\n':
			row = append(row, field.String())
			field.Reset()
			records = append(records, row)
			row = nil
		default:
			field.WriteByte(c)
		}
	}
	if inQuotes {
		return nil, fmt.Errorf("unterminated quoted field")
	}
	if field.Len() > 0 || len(row) > 0 {
		row = append(row, field.String())
		records = append(records, row)
	}
	return records, nil
}

func encodeField(s string) string {
	needsQuoting := false
	for _, r := range s {
		if r == ',' || r == '"' || r == '\n' || r == '\r' {
			needsQuoting = true
			break
		}
	}
	if !needsQuoting {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
