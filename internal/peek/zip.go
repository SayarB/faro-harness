package peek

import (
	"archive/zip"
	"context"
	"encoding/json"
	"strings"

	"github.com/SayarB/faro-harness/internal/store"
)

func peekZIP(_ context.Context, path string) Result {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	defer zr.Close()

	total := 0
	sample := make([]string, 0, min(len(zr.File), MaxZipEntries))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		total++
		if len(sample) < MaxZipEntries {
			sample = append(sample, f.Name)
		}
	}

	outlineBytes, _ := json.Marshal(struct {
		Type         string   `json:"type"`
		TotalEntries int      `json:"total_entries"`
		Sample       []string `json:"sample"`
	}{
		Type:         "zip",
		TotalEntries: total,
		Sample:       sample,
	})

	return Result{
		IndexDepth:  store.DepthHeaderOnly,
		IndexStatus: store.StatusOK,
		BodyPreview: truncateUTF8(strings.Join(sample, "\n"), MaxHeaderTextBytes),
		OutlineJSON: string(outlineBytes),
		UnitCount:   total,
	}
}
