package validators

import (
	"strconv"

	"github.com/lrills/helm-unittest/unittest/snapshot"
	"github.com/lrills/helm-unittest/unittest/valueutils"
)

// MatchSnapshotValidator validate snapshot of value of Path the same as cached
type MatchAllSnapshotsValidator struct {
	Path string
}

func (v MatchAllSnapshotsValidator) failInfo(compared *snapshot.CompareResult, not bool) []string {
	var notAnnotation = ""
	if not {
		notAnnotation = " NOT"
	}
	snapshotFailFormat := `
Path:%s
Expected` + notAnnotation + ` to match snapshot ` + strconv.Itoa(int(compared.Index)) + `:
%s
`
	var infoToShow string
	if not {
		infoToShow = compared.CachedSnapshot
	} else {
		infoToShow = diff(compared.CachedSnapshot, compared.NewSnapshot)
	}
	return splitInfof(snapshotFailFormat, v.Path, infoToShow)
}

// Validate implement Validatable
func (v MatchAllSnapshotsValidator) Validate(context *ValidateContext) (bool, []string) {

	var errors []string
	passed := true

	for _, manifest := range context.Docs {
		actual, err := valueutils.GetValueOfSetPath(manifest, v.Path)
		if err != nil {
			return false, splitInfof(errorFormat, err.Error())
		}
		result := context.CompareToSnapshot(actual)

		if result.Passed != context.Negative && result.CachedSnapshot != "" {
			continue
		}
		passed = false
		errors = append(errors, v.failInfo(result, context.Negative)...)

	}
	return passed, errors
}
