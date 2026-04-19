package utils

import (
	"bytes"
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
