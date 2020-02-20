package unittest

import (
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"path/filepath"
	"strings"

	"github.com/lrills/helm-unittest/unittest/common"
	"github.com/lrills/helm-unittest/unittest/snapshot"
	"github.com/lrills/helm-unittest/unittest/validators"
	"github.com/lrills/helm-unittest/unittest/valueutils"
	yaml "gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

type orderedSnapshotComparer struct {
	cache   *snapshot.Cache
	test    string
	counter uint
}

func (s *orderedSnapshotComparer) CompareToSnapshot(content interface{}) *snapshot.CompareResult {
	s.counter++
	return s.cache.Compare(s.test, s.counter, content)
}

// TestJob defintion of a test, including values and assertions
type TestJob struct {
	Name       string `yaml:"it"`
	Values     []string
	Set        map[string]interface{}
	Assertions []*Assertion `yaml:"asserts"`
	Release    struct {
		Name      string
		Namespace string
		Revision  int
		IsUpgrade bool
	}
	// route indicate which chart in the dependency hierarchy
	// like "parant-chart", "parent-charts/charts/child-chart"
	chartRoute string
	// where the test suite file located
	definitionFile string
	// template assertion should assert if not specified
	defaultTemplateToAssert string
}

// Run render the chart and validate it with assertions in TestJob
func (t *TestJob) Run(
	targetChart *chart.Chart,
	cache *snapshot.Cache,
	result *TestJobResult,
) *TestJobResult {
	t.polishAssertionsTemplate(targetChart)
	result.DisplayName = t.Name

	userValues, err := t.getUserValues()
	if err != nil {
		result.ExecError = err
		return result
	}

	outputOfFiles, err := t.renderChart(targetChart, userValues)
	if err != nil {
		result.ExecError = err
		return result
	}

	manifestsOfFiles, err := t.parseManifestsFromOutputOfFiles(outputOfFiles)
	if err != nil {
		result.ExecError = err
		return result
	}

	snapshotComparer := &orderedSnapshotComparer{cache: cache, test: t.Name}
	result.Passed, result.AssertsResult = t.runAssertions(
		manifestsOfFiles,
		snapshotComparer,
	)

	return result
}

// liberally borrows from helm-template
func (t *TestJob) getUserValues() (map[interface{}]interface{}, error) {
	base := map[interface{}]interface{}{}
	routes := spliteChartRoutes(t.chartRoute)

	for _, specifiedPath := range t.Values {
		value := map[interface{}]interface{}{}
		var valueFilePath string
		if path.IsAbs(specifiedPath) {
			valueFilePath = specifiedPath
		} else {
			valueFilePath = filepath.Join(filepath.Dir(t.definitionFile), specifiedPath)
		}

		bytes, err := ioutil.ReadFile(valueFilePath)
		if err != nil {
			return map[interface{}]interface{}{}, err
		}

		if err := yaml.Unmarshal(bytes, &value); err != nil {
			return map[interface{}]interface{}{}, fmt.Errorf("failed to parse %s: %s", specifiedPath, err)
		}
		base = valueutils.MergeValues(base, scopeValuesWithRoutes(routes, value))
	}

	for path, valus := range t.Set {
		setMap, err := valueutils.BuildValueOfSetPath(valus, path)
		if err != nil {
			return map[interface{}]interface{}{}, err
		}

		base = valueutils.MergeValues(base, scopeValuesWithRoutes(routes, setMap))
	}
	return base, nil
}
func cleanupInterfaceArray(in []interface{}) []interface{} {
	res := make([]interface{}, len(in))
	for i, v := range in {
		res[i] = cleanupMapValue(v)
	}
	return res
}

func cleanupInterfaceMap(in map[interface{}]interface{}) map[string]interface{} {
	res := make(map[string]interface{})
	for k, v := range in {
		res[fmt.Sprintf("%v", k)] = cleanupMapValue(v)
	}
	return res
}

func cleanupMapValue(v interface{}) interface{} {
	switch v := v.(type) {
	case []interface{}:
		return cleanupInterfaceArray(v)
	case map[interface{}]interface{}:
		return cleanupInterfaceMap(v)
	case string:
		return v
	default:
		return v
	}
}

// render the chart and return result map
func (t *TestJob) renderChart(targetChart *chart.Chart, userValues map[interface{}]interface{}) (map[string]string, error) {
	options := *t.releaseOption()

	values := cleanupMapValue(userValues).(map[string]interface{})

	vals, err := chartutil.ToRenderValues(targetChart, values, options, nil)
	if err != nil {
		return nil, err
	}

	outputOfFiles, err := engine.Render(targetChart, vals)
	if err != nil {
		return nil, err
	}

	return outputOfFiles, nil
}

// get chartutil.ReleaseOptions ready for render
func (t *TestJob) releaseOption() *chartutil.ReleaseOptions {
	options := chartutil.ReleaseOptions{
		Name:      "RELEASE-NAME",
		Namespace: "NAMESPACE",
		Revision:  t.Release.Revision,
		IsInstall: !t.Release.IsUpgrade,
		IsUpgrade: t.Release.IsUpgrade,
	}
	if t.Release.Name != "" {
		options.Name = t.Release.Name
	}
	if t.Release.Namespace != "" {
		options.Namespace = t.Release.Namespace
	}
	return &options
}

// parse rendered manifest if it's yaml
func (t *TestJob) parseManifestsFromOutputOfFiles(outputOfFiles map[string]string) (
	map[string][]common.K8sManifest,
	error,
) {
	manifestsOfFiles := make(map[string][]common.K8sManifest)

	for file, rendered := range outputOfFiles {
		decoder := yaml.NewDecoder(strings.NewReader(rendered))

		if filepath.Ext(file) == ".yaml" {
			manifests := make([]common.K8sManifest, 0)

			for {
				manifest := make(common.K8sManifest)
				if err := decoder.Decode(manifest); err != nil {
					if err == io.EOF {
						break
					} else {
						return nil, err
					}
				}

				if len(manifest) > 0 {
					manifests = append(manifests, manifest)
				}
			}

			manifestsOfFiles[file] = manifests
		}
	}

	return manifestsOfFiles, nil
}

// run Assert of all assertions of test
func (t *TestJob) runAssertions(
	manifestsOfFiles map[string][]common.K8sManifest,
	snapshotComparer validators.SnapshotComparer,
) (bool, []*AssertionResult) {
	testPass := true
	assertsResult := make([]*AssertionResult, len(t.Assertions))

	for idx, assertion := range t.Assertions {
		result := assertion.Assert(
			manifestsOfFiles,
			snapshotComparer,
			&AssertionResult{Index: idx},
		)

		assertsResult[idx] = result
		testPass = testPass && result.Passed
	}
	return testPass, assertsResult
}

// add prefix to Assertion.Template
func (t *TestJob) polishAssertionsTemplate(targetChart *chart.Chart) {
	if t.chartRoute == "" {
		t.chartRoute = targetChart.Metadata.Name
	}

	for _, assertion := range t.Assertions {
		var templateToAssert string

		if assertion.Template == "" {
			if t.defaultTemplateToAssert == "" {
				return
			}
			templateToAssert = t.defaultTemplateToAssert
		} else {
			templateToAssert = assertion.Template
		}

		// map the file name to the path of helm rendered result
		assertion.Template = filepath.ToSlash(
			filepath.Join(t.chartRoute, "templates", templateToAssert),
		)
	}
}
