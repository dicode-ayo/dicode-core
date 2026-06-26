package tasktest

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Individual test names in JUnit output are synthetic ("test N") because
// Deno's summary line carries only aggregate counts, not per-test details.
type junitTestsuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string              `xml:"name,attr"`
	Classname string              `xml:"classname,attr"`
	Time      string              `xml:"time,attr"`
	Failure   *junitFailure       `xml:"failure,omitempty"`
	Skipped   *junitSkippedMarker `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
}

type junitSkippedMarker struct{}

// FormatJUnit returns a JUnit XML string for r.
func FormatJUnit(r Result) string {
	secs := r.Duration.Seconds()
	n := r.Passed + r.Failed + r.Skipped
	perTest := 0.0
	if n > 0 {
		perTest = secs / float64(n)
	}

	var cases []junitCase
	for i := 0; i < r.Passed; i++ {
		cases = append(cases, junitCase{
			Name:      fmt.Sprintf("test %d", i+1),
			Classname: r.TaskID,
			Time:      fmt.Sprintf("%.3f", perTest),
		})
	}
	for i := 0; i < r.Failed; i++ {
		cases = append(cases, junitCase{
			Name:      fmt.Sprintf("test %d", r.Passed+i+1),
			Classname: r.TaskID,
			Time:      fmt.Sprintf("%.3f", perTest),
			Failure:   &junitFailure{Message: "test failed"},
		})
	}
	for i := 0; i < r.Skipped; i++ {
		cases = append(cases, junitCase{
			Name:      fmt.Sprintf("test %d", r.Passed+r.Failed+i+1),
			Classname: r.TaskID,
			Time:      fmt.Sprintf("%.3f", perTest),
			Skipped:   &junitSkippedMarker{},
		})
	}

	suites := junitTestsuites{Suites: []junitSuite{{
		Name:     r.TaskID,
		Tests:    r.Passed + r.Failed + r.Skipped,
		Failures: r.Failed,
		Skipped:  r.Skipped,
		Time:     fmt.Sprintf("%.3f", secs),
		Cases:    cases,
	}}}

	out, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		return fmt.Sprintf("<!-- xml marshal error: %v -->", err)
	}
	return xml.Header + string(out) + "\n"
}

// FormatGHSummary returns GitHub-flavoured Markdown for $GITHUB_STEP_SUMMARY.
func FormatGHSummary(r Result) string {
	icon := ":white_check_mark:"
	if r.Failed > 0 || r.ExitCode != 0 {
		icon = ":x:"
	}
	ms := r.Duration.Milliseconds()
	var b strings.Builder
	fmt.Fprintf(&b, "## %s `dicode task test %s`\n\n", icon, r.TaskID)
	fmt.Fprintf(&b, "| Passed | Failed | Skipped | Runtime | Duration |\n")
	fmt.Fprintf(&b, "|--------|--------|---------|---------|----------|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %s | %dms |\n\n", r.Passed, r.Failed, r.Skipped, r.Runtime, ms)
	if r.Output != "" {
		// Use tilde fencing so triple-backticks in test output don't break the block.
		fmt.Fprintf(&b, "<details><summary>Output</summary>\n\n~~~\n%s\n~~~\n\n</details>\n", r.Output)
	}
	return b.String()
}
