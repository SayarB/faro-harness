package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// SearchOptions configures structured lexical + trigram + metadata search over the catalog.
type SearchOptions struct {
	WorkspaceID   int64    // 0 searches all workspaces matching WorkspaceIDs / WorkspaceKind
	WorkspaceIDs  []int64  // Optional explicit allowlist of workspace IDs (takes precedence over WorkspaceID == 0)
	WorkspaceKind string   // Optional workspace kind filter ("user", "bot", "all"); defaults to "user" when no explicit workspace IDs are given
	Query         string   // Free-form query string (OR-ed by default unless MatchAll is true)
	Terms       []string // Additional query terms merged with Query
	ExactPhrase string   // Optional exact phrase that must appear in the match
	MatchAll    bool     // Require all terms (AND) instead of OR-by-default
	Ext         string   // Optional extension filter (e.g. ".xlsx" or "xlsx")
	Folder      string   // Optional relative folder prefix filter (e.g. "finance" matches "finance" and "finance/q4")
	MtimeAfter  int64    // Optional lower bound on mtime_ns (inclusive; 0 = unset)
	MtimeBefore int64    // Optional upper bound on mtime_ns (inclusive; 0 = unset)
	IndexDepth  string   // Optional index_depth filter ("full", "header_only", "metadata_only")
	Limit       int      // Max hits to return (default 20)
}

// SearchResult wraps ranked search hits along with truncation and query metadata.
type SearchResult struct {
	Hits         []SearchHit
	Truncated    bool
	FTSQuery     string
	TrigramQuery string
}

// queryUnit represents one user-entered term before identifier splitting so camelCase/letter-digit
// terms (e.g. "ClientInvoice", "March2026") match BOTH raw body text ("ClientInvoice"*) AND
// normalized filenames/folders ("Client"* AND "Invoice"*).
type queryUnit struct {
	raw   string   // original punctuation-free token (e.g. "ClientInvoice")
	parts []string // split sub-tokens (e.g. ["Client", "Invoice"])
}

var englishStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "by": {}, "for": {}, "from": {}, "has": {}, "have": {},
	"in": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {},
	"that": {}, "the": {}, "to": {}, "was": {}, "were": {}, "will": {}, "with": {},
}

// Search executes a weighted BM25 FTS5 + trigram query with default OR semantics.
// Pass workspaceID == 0 to search all workspaces. Returns nil for an empty query.
func (s *Store) Search(ctx context.Context, workspaceID int64, rawQuery string, limit int) ([]SearchHit, error) {
	if strings.TrimSpace(rawQuery) == "" {
		return nil, nil
	}
	res, err := s.SearchWithOptions(ctx, SearchOptions{
		WorkspaceID: workspaceID,
		Query:       rawQuery,
		Limit:       limit,
	})
	if err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// SearchWithOptions runs a structured search supporting forgiving OR queries, stopwords,
// exact phrases, strict MatchAll (AND), trigram filename/folder substring matching,
// and metadata filters (or recency-sorted filter-only queries when no text query is given).
func (s *Store) SearchWithOptions(ctx context.Context, opts SearchOptions) (SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}

	rawQuery := strings.TrimSpace(opts.Query)
	exactPhrase := strings.TrimSpace(opts.ExactPhrase)
	hasRawTerms := false
	for _, t := range opts.Terms {
		if strings.TrimSpace(t) != "" {
			hasRawTerms = true
			break
		}
	}

	// Only take the filter-only recency path when no text query, terms, or phrase were provided.
	// Punctuation-only queries (e.g. "!!!", "-") must NOT dump the entire catalog.
	if rawQuery == "" && !hasRawTerms && exactPhrase == "" {
		return s.searchFilterOnly(ctx, opts)
	}

	units := collectQueryUnits(rawQuery, opts.Terms)
	ftsUnits := stripStopwordUnitsWithFallback(units)
	ftsQuery := buildFTSQuery(ftsUnits, exactPhrase, opts.MatchAll)

	rawTrigramTerms := collectRawTrigramTerms(rawQuery, opts.Terms)
	trigramQuery := buildTrigramQuery(rawTrigramTerms, exactPhrase, opts.MatchAll)

	if ftsQuery == "" && trigramQuery == "" {
		return SearchResult{}, nil
	}

	fetchLimit := opts.Limit + 1

	var ftsHits []SearchHit
	var ftsTruncated bool
	if ftsQuery != "" {
		var err error
		ftsHits, err = s.queryFTS(ctx, opts, ftsQuery, fetchLimit)
		if err != nil {
			return SearchResult{}, err
		}
		if len(ftsHits) > opts.Limit {
			ftsTruncated = true
			ftsHits = ftsHits[:opts.Limit]
		}
	}

	var trigramHits []SearchHit
	var trigramTruncated bool
	if trigramQuery != "" {
		var err error
		trigramHits, err = s.queryTrigram(ctx, opts, trigramQuery, fetchLimit)
		if err != nil {
			return SearchResult{}, err
		}
		if len(trigramHits) > opts.Limit {
			trigramTruncated = true
			trigramHits = trigramHits[:opts.Limit]
		}
	}

	merged, mergeTruncated := mergeSearchHits(ftsHits, trigramHits, opts.Limit)
	return SearchResult{
		Hits:         merged,
		Truncated:    ftsTruncated || trigramTruncated || mergeTruncated,
		FTSQuery:     ftsQuery,
		TrigramQuery: trigramQuery,
	}, nil
}

func (s *Store) searchFilterOnly(ctx context.Context, opts SearchOptions) (SearchResult, error) {
	whereSQL, args := buildFilterClauses(opts)
	query := `
		SELECT
			f.id, f.workspace_id, f.rel_path, f.filename, f.folder, f.ext,
			f.size, f.mtime_ns, f.index_depth, f.index_status,
			COALESCE(b.title, ''), COALESCE(b.headings, '')
		FROM files f
		JOIN workspaces w ON w.id = f.workspace_id
		LEFT JOIN content_blobs b
			ON b.blake3 = f.blake3
			AND b.peeker_id = f.peeker_id
			AND b.peeker_version = f.peeker_version
	`
	if whereSQL != "" {
		query += ` WHERE ` + whereSQL
	}
	query += ` ORDER BY f.mtime_ns DESC, f.id DESC LIMIT ?`
	args = append(args, opts.Limit+1)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResult{}, fmt.Errorf("filter search: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.FileID, &h.WorkspaceID, &h.RelPath, &h.Filename, &h.Folder, &h.Ext,
			&h.Size, &h.MtimeNS, &h.IndexDepth, &h.IndexStatus,
			&h.Title, &h.Headings,
		); err != nil {
			return SearchResult{}, err
		}
		h.MatchSource = "filter"
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, err
	}

	truncated := false
	if len(hits) > opts.Limit {
		truncated = true
		hits = hits[:opts.Limit]
	}
	return SearchResult{Hits: hits, Truncated: truncated}, nil
}

func (s *Store) queryFTS(ctx context.Context, opts SearchOptions, ftsQuery string, limit int) ([]SearchHit, error) {
	filterSQL, filterArgs := buildFilterClauses(opts)
	query := `
		SELECT
			f.id, f.workspace_id, f.rel_path, f.filename, f.folder, f.ext,
			f.size, f.mtime_ns, f.index_depth, f.index_status,
			COALESCE(b.title, ''), COALESCE(b.headings, ''),
			snippet(files_fts, -1, '[', ']', '…', 16),
			bm25(files_fts, 10.0, 5.0, 3.0, 1.0) AS rank
		FROM files_fts
		JOIN files f ON f.id = files_fts.rowid
		JOIN workspaces w ON w.id = f.workspace_id
		LEFT JOIN content_blobs b
			ON b.blake3 = f.blake3
			AND b.peeker_id = f.peeker_id
			AND b.peeker_version = f.peeker_version
		WHERE files_fts MATCH ?
	`
	args := []any{ftsQuery}
	if filterSQL != "" {
		query += ` AND ` + filterSQL
		args = append(args, filterArgs...)
	}
	query += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fts search %q: %w", ftsQuery, err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.FileID, &h.WorkspaceID, &h.RelPath, &h.Filename, &h.Folder, &h.Ext,
			&h.Size, &h.MtimeNS, &h.IndexDepth, &h.IndexStatus,
			&h.Title, &h.Headings, &h.Snippet, &h.Rank,
		); err != nil {
			return nil, err
		}
		h.MatchSource = "fts"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) queryTrigram(ctx context.Context, opts SearchOptions, trigramQuery string, limit int) ([]SearchHit, error) {
	filterSQL, filterArgs := buildFilterClauses(opts)
	query := `
		SELECT
			f.id, f.workspace_id, f.rel_path, f.filename, f.folder, f.ext,
			f.size, f.mtime_ns, f.index_depth, f.index_status,
			COALESCE(b.title, ''), COALESCE(b.headings, ''),
			snippet(files_trigram, -1, '[', ']', '…', 16),
			bm25(files_trigram, 10.0, 3.0) AS rank
		FROM files_trigram
		JOIN files f ON f.id = files_trigram.rowid
		JOIN workspaces w ON w.id = f.workspace_id
		LEFT JOIN content_blobs b
			ON b.blake3 = f.blake3
			AND b.peeker_id = f.peeker_id
			AND b.peeker_version = f.peeker_version
		WHERE files_trigram MATCH ?
	`
	args := []any{trigramQuery}
	if filterSQL != "" {
		query += ` AND ` + filterSQL
		args = append(args, filterArgs...)
	}
	query += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("trigram search %q: %w", trigramQuery, err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.FileID, &h.WorkspaceID, &h.RelPath, &h.Filename, &h.Folder, &h.Ext,
			&h.Size, &h.MtimeNS, &h.IndexDepth, &h.IndexStatus,
			&h.Title, &h.Headings, &h.Snippet, &h.Rank,
		); err != nil {
			return nil, err
		}
		h.MatchSource = "trigram"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func buildFilterClauses(opts SearchOptions) (string, []any) {
	var clauses []string
	var args []any

	if opts.WorkspaceID > 0 {
		clauses = append(clauses, "f.workspace_id = ?")
		args = append(args, opts.WorkspaceID)
	} else if len(opts.WorkspaceIDs) > 0 {
		placeholders := make([]string, len(opts.WorkspaceIDs))
		for i, id := range opts.WorkspaceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		clauses = append(clauses, "f.workspace_id IN ("+strings.Join(placeholders, ",")+")")
	}
	kind := strings.TrimSpace(strings.ToLower(opts.WorkspaceKind))
	if kind == "" && opts.WorkspaceID == 0 && len(opts.WorkspaceIDs) == 0 {
		kind = WorkspaceKindUser
	}
	if kind != "" && kind != "all" {
		clauses = append(clauses, "w.kind = ?")
		args = append(args, kind)
	}
	if ext := strings.TrimSpace(strings.ToLower(opts.Ext)); ext != "" {
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		clauses = append(clauses, "f.ext = ?")
		args = append(args, ext)
	}
	if folder := strings.Trim(filepath.ToSlash(strings.TrimSpace(opts.Folder)), "/"); folder != "" && folder != "." {
		clauses = append(clauses, "(f.folder = ? OR (f.folder >= ? AND f.folder < ?))")
		args = append(args, folder, folder+"/", folder+"0")
	}
	if opts.MtimeAfter > 0 {
		clauses = append(clauses, "f.mtime_ns >= ?")
		args = append(args, opts.MtimeAfter)
	}
	if opts.MtimeBefore > 0 {
		clauses = append(clauses, "f.mtime_ns <= ?")
		args = append(args, opts.MtimeBefore)
	}
	if depth := strings.TrimSpace(opts.IndexDepth); depth != "" {
		clauses = append(clauses, "f.index_depth = ?")
		args = append(args, depth)
	}

	return strings.Join(clauses, " AND "), args
}

// mergeSearchHits combines primary FTS5 hits and trigram substring hits.
// Files matching both keep the FTS5 rank/snippet and get MatchSource="fts+trigram".
// Primary FTS5 hits sort ahead of trigram-only substring fallbacks, each ordered by BM25 rank.
func mergeSearchHits(ftsHits, trigramHits []SearchHit, limit int) ([]SearchHit, bool) {
	byID := make(map[int64]int, len(ftsHits))
	merged := make([]SearchHit, 0, len(ftsHits)+len(trigramHits))

	for _, h := range ftsHits {
		byID[h.FileID] = len(merged)
		merged = append(merged, h)
	}

	var trigramOnly []SearchHit
	for _, th := range trigramHits {
		if idx, exists := byID[th.FileID]; exists {
			merged[idx].MatchSource = "fts+trigram"
			continue
		}
		trigramOnly = append(trigramOnly, th)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Rank < merged[j].Rank
	})
	sort.SliceStable(trigramOnly, func(i, j int) bool {
		return trigramOnly[i].Rank < trigramOnly[j].Rank
	})

	merged = append(merged, trigramOnly...)
	truncated := false
	if len(merged) > limit {
		truncated = true
		merged = merged[:limit]
	}
	return merged, truncated
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r)
}

// normalizeForFTS splits separators, camelCase, acronym boundaries (HTTPServer -> HTTP Server,
// while preserving plural acronyms like PDFs/IDs/URLs), and letter/digit boundaries
// (March2026 -> March 2026, 2026Report -> 2026 Report) while preserving compact codes
// like Q4, v2, and FY26, and keeping Unicode combining marks (e.g. Indic scripts) intact.
func normalizeForFTS(s string) string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, splitIdentifierTokens(cur)...)
			cur = cur[:0]
		}
	}

	for _, r := range s {
		if isTokenRune(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(parts, " ")
}

func splitIdentifierTokens(runes []rune) []string {
	if len(runes) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		curr := runes[i]
		split := false

		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(curr):
			// camelCase: ClientInvoice -> Client Invoice
			split = true
		case unicode.IsUpper(prev) && unicode.IsUpper(curr) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			// Acronym boundary: HTTPServer -> HTTP Server, PDFReport -> PDF Report.
			// Do NOT split plural acronyms like PDFs, IDs, URLs, URLsList (single trailing lowercase 's').
			isPluralS := runes[i+1] == 's' && (i+2 == len(runes) || !unicode.IsLower(runes[i+2]))
			if !isPluralS {
				split = true
			}
		case unicode.IsDigit(prev) && unicode.IsLetter(curr):
			// Digit to letter: 2026Report -> 2026 Report, Q4Forecast -> Q4 Forecast
			split = true
		case unicode.IsLetter(prev) && unicode.IsDigit(curr):
			// Letter to digit: split when letter run >= 3 (March2026, Invoice2) or digit run >= 4 (FY2026),
			// preserving short codes like Q4, v2, FY26.
			letterRun := i - start
			digitRun := 0
			for j := i; j < len(runes) && unicode.IsDigit(runes[j]); j++ {
				digitRun++
			}
			if letterRun >= 3 || digitRun >= 4 {
				split = true
			}
		}

		if split {
			out = append(out, string(runes[start:i]))
			start = i
		}
	}
	out = append(out, string(runes[start:]))
	return out
}

func collectQueryUnits(rawQuery string, extraTerms []string) []queryUnit {
	var units []queryUnit
	seen := make(map[string]struct{})

	addText := func(s string) {
		var cur []rune
		flush := func() {
			if len(cur) == 0 {
				return
			}
			raw := string(cur)
			cur = cur[:0]
			lower := strings.ToLower(raw)
			if _, ok := seen[lower]; ok {
				return
			}
			seen[lower] = struct{}{}
			units = append(units, queryUnit{
				raw:   raw,
				parts: splitIdentifierTokens([]rune(raw)),
			})
		}
		for _, r := range s {
			if isTokenRune(r) {
				cur = append(cur, r)
			} else {
				flush()
			}
		}
		flush()
	}

	addText(rawQuery)
	for _, t := range extraTerms {
		addText(t)
	}
	return units
}

func stripStopwordUnitsWithFallback(units []queryUnit) []queryUnit {
	if len(units) == 0 {
		return nil
	}
	filtered := make([]queryUnit, 0, len(units))
	for _, u := range units {
		if _, isStop := englishStopwords[strings.ToLower(u.raw)]; !isStop {
			filtered = append(filtered, u)
		}
	}
	if len(filtered) == 0 {
		return units
	}
	return filtered
}

func collectRawTrigramTerms(rawQuery string, extraTerms []string) []string {
	var rawWords []string
	seen := make(map[string]struct{})
	add := func(s string) {
		for _, w := range strings.Fields(s) {
			w = strings.TrimSpace(strings.ReplaceAll(w, `"`, ""))
			if w == "" {
				continue
			}
			lower := strings.ToLower(w)
			if _, ok := seen[lower]; ok {
				continue
			}
			seen[lower] = struct{}{}
			rawWords = append(rawWords, w)
		}
	}
	add(rawQuery)
	for _, t := range extraTerms {
		add(t)
	}
	if len(rawWords) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(rawWords))
	for _, w := range rawWords {
		if _, isStop := englishStopwords[strings.ToLower(w)]; !isStop {
			filtered = append(filtered, w)
		}
	}
	if len(filtered) == 0 {
		return rawWords
	}
	return filtered
}

func quoteFTSToken(tok string, allowPrefix bool) string {
	tok = strings.ReplaceAll(tok, `"`, "")
	if tok == "" {
		return ""
	}
	if allowPrefix && len([]rune(tok)) >= 3 {
		return `"` + tok + `"*`
	}
	return `"` + tok + `"`
}

func quoteFTSPhrase(phrase string) string {
	norm := strings.TrimSpace(normalizeForFTS(phrase))
	if norm == "" {
		return ""
	}
	// Preserve raw whitespace-collapsed form alongside normalized form if they differ
	// (e.g. camelCase inside ExactPhrase matches both raw body text and split filenames).
	rawWords := extractRawWords(phrase)
	rawJoined := strings.Join(rawWords, " ")
	if rawJoined != "" && !strings.EqualFold(rawJoined, norm) {
		return `("` + rawJoined + `" OR "` + norm + `")`
	}
	return `"` + norm + `"`
}

func extractRawWords(s string) []string {
	var words []string
	var cur []rune
	for _, r := range s {
		if isTokenRune(r) {
			cur = append(cur, r)
		} else if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

func buildUnitFTSExpr(u queryUnit) string {
	if len(u.parts) <= 1 {
		return quoteFTSToken(u.raw, true)
	}
	// Compound identifier (e.g. ClientInvoice, March2026): match EITHER the unsplit token
	// in raw body/headings text OR all split pieces in normalized filename/folder.
	rawTok := quoteFTSToken(u.raw, true)
	partExprs := make([]string, 0, len(u.parts))
	for _, p := range u.parts {
		if q := quoteFTSToken(p, true); q != "" {
			partExprs = append(partExprs, q)
		}
	}
	if len(partExprs) == 0 {
		return rawTok
	}
	return "(" + rawTok + " OR (" + strings.Join(partExprs, " AND ") + "))"
}

func buildFTSQuery(units []queryUnit, exactPhrase string, matchAll bool) string {
	var phraseExpr string
	if exactPhrase != "" {
		phraseExpr = quoteFTSPhrase(exactPhrase)
		// If the caller explicitly requested an ExactPhrase that normalized to nothing
		// (e.g. "---"), do NOT drop the phrase constraint and return unconstrained hits.
		if phraseExpr == "" {
			return ""
		}
	}

	var termExpr string
	if len(units) > 0 {
		parts := make([]string, 0, len(units))
		for _, u := range units {
			if expr := buildUnitFTSExpr(u); expr != "" {
				parts = append(parts, expr)
			}
		}
		if len(parts) > 0 {
			joiner := " OR "
			if matchAll {
				joiner = " AND "
			}
			termExpr = strings.Join(parts, joiner)
		}
	}

	if phraseExpr != "" && termExpr != "" {
		return "(" + phraseExpr + ") AND (" + termExpr + ")"
	}
	if phraseExpr != "" {
		return phraseExpr
	}
	return termExpr
}

// buildTrigramQuery builds an FTS5 trigram query over raw filename and folder.
// SQLite's trigram tokenizer requires terms of at least 3 runes.
// When matchAll is true and any required term is shorter than 3 runes, returns "" so
// trigram search never emits hits that violate MatchAll.
func buildTrigramQuery(rawTerms []string, exactPhrase string, matchAll bool) string {
	var termExpr string
	if len(rawTerms) > 0 {
		parts := make([]string, 0, len(rawTerms))
		for _, t := range rawTerms {
			t = strings.ReplaceAll(t, `"`, "")
			if len([]rune(t)) < 3 {
				if matchAll {
					return ""
				}
				continue
			}
			parts = append(parts, `"`+t+`"`)
		}
		if len(parts) > 0 {
			joiner := " OR "
			if matchAll {
				joiner = " AND "
			}
			termExpr = strings.Join(parts, joiner)
		}
	}

	phrase := strings.TrimSpace(strings.ReplaceAll(exactPhrase, `"`, " "))
	var phraseExpr string
	if len([]rune(phrase)) >= 3 {
		phraseExpr = `"` + phrase + `"`
	} else if exactPhrase != "" {
		return ""
	}

	if phraseExpr != "" && termExpr != "" {
		return "(" + phraseExpr + ") AND (" + termExpr + ")"
	}
	if phraseExpr != "" {
		return phraseExpr
	}
	return termExpr
}
