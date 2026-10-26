package domain

import (
	"sort"
	"time"
)

type ConversionStatus string

const (
	ConversionQueued       ConversionStatus = "queued"
	ConversionProcessing   ConversionStatus = "processing"
	ConversionReady        ConversionStatus = "ready"
	ConversionPartial      ConversionStatus = "partial"
	ConversionNotConverted ConversionStatus = "not_converted"
	ConversionBlocked      ConversionStatus = "blocked"
	ConversionFailed       ConversionStatus = "failed"
)

type PageStage string

const (
	StageText        PageStage = "text"
	StageDescription PageStage = "description"
)

type PageResult struct {
	Page      int
	Stage     PageStage
	Attempt   int64
	Succeeded bool
	ResultRef string
	ErrorCode string
}
type Completeness struct {
	Status                  ConversionStatus `json:"status"`
	TotalPages              int              `json:"total_pages"`
	MissingTextPages        []int            `json:"missing_text_pages"`
	MissingDescriptionPages []int            `json:"missing_description_pages"`
	Warnings                []string         `json:"warnings,omitempty"`
}

// ComputeCompleteness includes pages with no recorded result. Missing or failed
// description is not hidden merely because OCR succeeded. Only the latest page
// attempt contributes; older successful attempts cannot disguise a new failure.
func ComputeCompleteness(totalPages int, descriptionPages []int, results []PageResult) (Completeness, error) {
	c := Completeness{TotalPages: totalPages, MissingTextPages: []int{}, MissingDescriptionPages: []int{}}
	if totalPages < 1 || totalPages > 1000000 {
		return c, NewError(InvalidArgument, "invalid total page count")
	}
	desc := map[int]bool{}
	for _, p := range descriptionPages {
		if p < 1 || p > totalPages {
			return c, NewError(InvalidArgument, "description page is outside the document")
		}
		desc[p] = true
	}
	type key struct {
		page  int
		stage PageStage
	}
	latest := map[key]PageResult{}
	for _, r := range results {
		if r.Page < 1 || r.Page > totalPages || r.Attempt < 1 || (r.Stage != StageText && r.Stage != StageDescription) {
			return c, NewError(InvalidArgument, "invalid page result")
		}
		if r.Succeeded && (r.ResultRef == "" || r.ErrorCode != "") {
			return c, NewError(InvalidArgument, "successful page must have a durable result and no error")
		}
		k := key{r.Page, r.Stage}
		prev, ok := latest[k]
		if ok && prev.Attempt == r.Attempt && prev != r {
			return c, NewError(IdempotencyConflict, "page attempt has conflicting results")
		}
		if !ok || r.Attempt > prev.Attempt {
			latest[k] = r
		}
	}
	for p := 1; p <= totalPages; p++ {
		if !latest[key{p, StageText}].Succeeded {
			c.MissingTextPages = append(c.MissingTextPages, p)
		}
		if desc[p] && !latest[key{p, StageDescription}].Succeeded {
			c.MissingDescriptionPages = append(c.MissingDescriptionPages, p)
		}
	}
	switch {
	case len(c.MissingTextPages) == totalPages:
		c.Status = ConversionFailed
	case len(c.MissingTextPages) > 0 || len(c.MissingDescriptionPages) > 0:
		c.Status = ConversionPartial
	default:
		c.Status = ConversionReady
	}
	if len(c.MissingTextPages) > 0 {
		c.Warnings = append(c.Warnings, "incomplete_text")
	}
	if len(c.MissingDescriptionPages) > 0 {
		c.Warnings = append(c.Warnings, "incomplete_description")
	}
	return c, nil
}

func (c Completeness) Validate() error {
	if c.TotalPages < 1 {
		return NewError(InvalidArgument, "completeness requires a positive page count")
	}
	for _, pages := range [][]int{c.MissingTextPages, c.MissingDescriptionPages} {
		seen := map[int]bool{}
		for _, p := range pages {
			if p < 1 || p > c.TotalPages || seen[p] {
				return NewError(InvalidArgument, "invalid missing-page map")
			}
			seen[p] = true
		}
	}
	missing := len(c.MissingTextPages) > 0 || len(c.MissingDescriptionPages) > 0
	if (c.Status == ConversionReady && missing) || (c.Status == ConversionPartial && (!missing || len(c.MissingTextPages) == c.TotalPages)) || (c.Status != ConversionReady && c.Status != ConversionPartial) {
		return NewError(InvalidArgument, "only truthful ready or partial artifacts can be published")
	}
	return nil
}

type LeaseFence struct {
	Attempt int64
	Token   int64
	Until   time.Time
}

func CheckLease(current LeaseFence, attempt, token int64, now time.Time) error {
	if attempt < 1 || token < 1 || current.Attempt != attempt || current.Token != token || now.IsZero() || !now.Before(current.Until) {
		return NewError(LeaseLost, "worker attempt is stale or its lease has expired")
	}
	return nil
}

type PublicationState struct {
	CurrentVersionSequence    int64
	SearchVersionSequence     int64
	SearchGeneration          int64
	LatestRequestedGeneration int64 // For the candidate's source version.
	Lease                     LeaseFence
}
type PublicationCandidate struct {
	VersionSequence int64
	Generation      int64
	Attempt         int64
	LeaseToken      int64
	IndexState      string
	Completeness    Completeness
}

// ValidatePublication permits an older source to become searchable while a newer
// source is queued, but never replaces a newer published source with an older
// result. Within one source, only the latest requested generation may publish.
// The same conditions must be compared inside the pointer-update transaction.
func ValidatePublication(current PublicationState, candidate PublicationCandidate, now time.Time) error {
	if err := CheckLease(current.Lease, candidate.Attempt, candidate.LeaseToken, now); err != nil {
		return err
	}
	if candidate.IndexState != "succeeded" {
		return NewError(OperationPending, "indexing must actually succeed before publishing")
	}
	if err := candidate.Completeness.Validate(); err != nil {
		return err
	}
	if candidate.VersionSequence < 1 || candidate.Generation < 1 || candidate.VersionSequence > current.CurrentVersionSequence || candidate.Generation != current.LatestRequestedGeneration {
		return NewError(PublicationSuperseded, "a newer conversion was requested or the source is invalid")
	}
	if candidate.VersionSequence < current.SearchVersionSequence || (candidate.VersionSequence == current.SearchVersionSequence && candidate.Generation <= current.SearchGeneration) {
		return NewError(PublicationSuperseded, "publication would regress or repeat the current artifact pointer")
	}
	return nil
}

// MissingPages returns a sorted union for clients needing a compact warning.
func (c Completeness) MissingPages() []int {
	m := map[int]bool{}
	for _, p := range c.MissingTextPages {
		m[p] = true
	}
	for _, p := range c.MissingDescriptionPages {
		m[p] = true
	}
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
