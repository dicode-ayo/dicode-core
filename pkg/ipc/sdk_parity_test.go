package ipc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	shimPath   = "../runtime/deno/sdk/shim.ts"
	sdkDtsPath = "../runtime/deno/sdk/sdk.d.ts"
	pySDKPath  = "../runtime/python/sdk/dicode_sdk.py"
)

// sdkExemptions lists dispatched verbs an SDK legitimately does not send,
// keyed by verb then by SDK file. Every entry carries its reason.
var sdkExemptions = map[string]map[string]string{
	"http.register": {
		shimPath:  "daemon HTTP plumbing; no SDK surface yet",
		pySDKPath: "daemon HTTP plumbing; no SDK surface yet",
	},
	"http.respond": {
		shimPath:  "daemon HTTP plumbing; no SDK surface yet",
		pySDKPath: "daemon HTTP plumbing; no SDK surface yet",
	},
	"log": {
		shimPath: "Deno tasks log via console.*, which the runtime streams from stdout/stderr",
	},
}

// dispatchedVerbs returns the case string literals of every `switch req.Method`
// inside (*Server).handleConn.
func dispatchedVerbs(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var handle *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleConn" && fd.Recv != nil {
			handle = fd
		}
	}
	if handle == nil {
		t.Fatal("handleConn not found in server.go")
	}
	verbs := map[string]bool{}
	ast.Inspect(handle.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		sel, ok := sw.Tag.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Method" {
			return true
		}
		for _, st := range sw.Body.List {
			for _, e := range st.(*ast.CaseClause).List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}
					verbs[v] = true
				}
			}
		}
		return true
	})
	if len(verbs) < 10 {
		t.Fatalf("found only %d verbs in handleConn's method switch; the AST walk is broken", len(verbs))
	}
	return verbs
}

var sentMethodRe = map[string]*regexp.Regexp{
	shimPath:  regexp.MustCompile(`\bmethod:\s*"([^"]+)"`),
	pySDKPath: regexp.MustCompile(`"method":\s*"([^"]+)"`),
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func sentMethods(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range sentMethodRe[path].FindAllStringSubmatch(readFile(t, path), -1) {
		out[m[1]] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSDKParity_EveryDispatchedVerbHasSDKMethod(t *testing.T) {
	verbs := dispatchedVerbs(t)
	for verb, sdks := range sdkExemptions {
		if !verbs[verb] {
			t.Errorf("exemption %q is not dispatched by handleConn; drop it", verb)
		}
		for sdk, reason := range sdks {
			if reason == "" {
				t.Errorf("exemption %q for %s has no reason", verb, sdk)
			}
		}
	}
	for _, sdk := range []string{shimPath, pySDKPath} {
		sent := sentMethods(t, sdk)
		for _, verb := range sortedKeys(verbs) {
			if _, exempt := sdkExemptions[verb][sdk]; exempt {
				if sent[verb] {
					t.Errorf("%s sends %q but it is exempted for that SDK; drop the exemption", sdk, verb)
				}
				continue
			}
			if !sent[verb] {
				t.Errorf("%s has no method sending %q, which handleConn dispatches", sdk, verb)
			}
		}
	}
}

func TestSDKParity_EverySentVerbIsDispatched(t *testing.T) {
	verbs := dispatchedVerbs(t)
	for _, sdk := range []string{shimPath, pySDKPath} {
		for _, m := range sortedKeys(sentMethods(t, sdk)) {
			if !verbs[m] {
				t.Errorf("%s sends %q, which handleConn does not dispatch", sdk, m)
			}
		}
	}
}

// TestSDKParity_SdkDtsDeclaresShimDicode asserts that the Monaco declarations
// declare every member (nested namespace keys included) of each Dicode*
// interface in shim.ts.
func TestSDKParity_SdkDtsDeclaresShimDicode(t *testing.T) {
	shim := readFile(t, shimPath)
	dts := readFile(t, sdkDtsPath)
	names := regexp.MustCompile(`\binterface\s+(Dicode\w*)\s*\{`).FindAllStringSubmatch(shim, -1)
	if len(names) == 0 {
		t.Fatal("no Dicode* interfaces found in shim.ts")
	}
	for _, n := range names {
		name := n[1]
		want := interfaceMembers(t, shim, name)
		if len(want) == 0 {
			t.Fatalf("shim.ts interface %s has no members; the scanner is broken", name)
		}
		if !regexp.MustCompile(`\bdeclare\s+interface\s+` + name + `\s*\{`).MatchString(dts) {
			t.Errorf("sdk.d.ts has no `declare interface %s`", name)
			continue
		}
		got := interfaceMembers(t, dts, name)
		for _, m := range sortedKeys(want) {
			if !got[m] {
				t.Errorf("sdk.d.ts %s is missing member %q declared in shim.ts", name, m)
			}
		}
	}
}

func TestInterfaceMembersScanner(t *testing.T) {
	src := `interface Dicode {
  // a: comment
  task_id: string;
  run_task: (id: string, opts?: { mcpContext?: boolean }) => Promise<unknown>;
  /* skipped: x */
  runs: {
    list?: (opts?: { before_ts?: number }) => Promise<{ inner: string }>;
  };
}
interface DicodeOther { z: string }`
	got := sortedKeys(interfaceMembers(t, src, "Dicode"))
	want := []string{"run_task", "runs", "runs.list", "task_id"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

var commentRe = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)

// interfaceMembers returns the dotted member paths of `interface <name> {…}`
// in src. Keys inside parameter lists and generic arguments are ignored, so
// only members and nested object-type namespaces are reported.
func interfaceMembers(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	src = commentRe.ReplaceAllString(src, "")
	loc := regexp.MustCompile(`\binterface\s+` + name + `\s*\{`).FindStringIndex(src)
	if loc == nil {
		return nil
	}
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	out := map[string]bool{}
	path := []string{}
	lastKey := ""
	paren, angle := 0, 0
	for i := loc[1]; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '(':
			paren++
		case c == ')':
			paren--
		case c == '=' && i+1 < len(src) && src[i+1] == '>':
			i++
		case c == '<':
			angle++
		case c == '>':
			angle--
		case c == '{' && paren == 0 && angle == 0:
			path = append(path, lastKey)
			lastKey = ""
		case c == '}' && paren == 0 && angle == 0:
			if len(path) == 0 {
				return out
			}
			path = path[:len(path)-1]
		case isIdent(c) && paren == 0 && angle == 0 && (i == 0 || !isIdent(src[i-1])):
			j := i
			for j < len(src) && isIdent(src[j]) {
				j++
			}
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n') {
				k++
			}
			if k < len(src) && src[k] == '?' {
				k++
			}
			if k < len(src) && src[k] == ':' {
				key := strings.Join(append(append([]string{}, path...), src[i:j]), ".")
				out[key] = true
				lastKey = src[i:j]
				i = k
			} else {
				i = j - 1
			}
		}
	}
	t.Fatalf("unterminated interface %s", name)
	return nil
}
