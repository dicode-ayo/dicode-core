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
	strip := stripTSComments
	if strings.HasSuffix(path, ".py") {
		strip = stripPyComments
	}
	return scanMethods(sentMethodRe[path], strip(readFile(t, path)))
}

func scanMethods(re *regexp.Regexp, src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// blank writes s to b with every byte but newlines turned into a space, so
// offsets and line numbers survive stripping.
func blank(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
	}
}

// quotedEnd returns the index just past the string literal that opens at i
// with quote q, honouring backslash escapes. Unterminated literals run to EOF.
func quotedEnd(src string, i int, q string) int {
	for j := i + len(q); j < len(src); j++ {
		if src[j] == '\\' {
			j++
			continue
		}
		if strings.HasPrefix(src[j:], q) {
			return j + len(q)
		}
	}
	return len(src)
}

// stripTSComments blanks // and /* */ comments, leaving string and template
// literals intact. Regex literals are not recognised.
func stripTSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = len(src) - i
			}
			blank(&b, src[i:i+j])
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			end := len(src)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			blank(&b, src[i:end])
			i = end
		case src[i] == '"' || src[i] == '\'' || src[i] == '`':
			end := quotedEnd(src, i, src[i:i+1])
			b.WriteString(src[i:end])
			i = end
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// stripPyComments blanks # comments and docstrings (triple-quoted strings
// that open a line), leaving every other string literal intact.
func stripPyComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case src[i] == '#':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = len(src) - i
			}
			blank(&b, src[i:i+j])
			i += j
		case strings.HasPrefix(src[i:], `"""`) || strings.HasPrefix(src[i:], "'''"):
			end := quotedEnd(src, i, src[i:i+3])
			lineStart := strings.LastIndexByte(src[:i], '\n') + 1
			if strings.TrimSpace(src[lineStart:i]) == "" {
				blank(&b, src[i:end])
			} else {
				b.WriteString(src[i:end])
			}
			i = end
		case src[i] == '"' || src[i] == '\'':
			end := quotedEnd(src, i, src[i:i+1])
			b.WriteString(src[i:end])
			i = end
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func TestSentMethodsScannerIgnoresComments(t *testing.T) {
	ts := `// _call({ method: "ts.line" })
/* _call({ method: "ts.block" }) */
const u = "http://x"; _call({ method: "ts.real" }); // method: "ts.trailing"
const v = '/* not a comment */'; _call({ method: "ts.after_string" });`
	got := strings.Join(sortedKeys(scanMethods(sentMethodRe[shimPath], stripTSComments(ts))), ",")
	if want := "ts.after_string,ts.real"; got != want {
		t.Errorf("TS: got %s, want %s", got, want)
	}

	py := `def f():
    """Docstring: {"method": "py.docstring"}"""
    # _call({"method": "py.comment"})
    tag = "#not-a-comment"
    _call({"method": "py.real", "tag": tag})  # "method": "py.trailing"
    s = """inline {"method": "py.inline_triple"}"""
`
	got = strings.Join(sortedKeys(scanMethods(sentMethodRe[pySDKPath], stripPyComments(py))), ",")
	if want := "py.inline_triple,py.real"; got != want {
		t.Errorf("Python: got %s, want %s", got, want)
	}
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

// shimInternalInterfaces are shim.ts interfaces with no public SDK type, so
// sdk.d.ts need not declare them. Every entry carries its reason.
var shimInternalInterfaces = map[string]string{
	"IpcRequest":        "IPC wire frame, never handed to task code",
	"IpcResponse":       "IPC wire frame, never handed to task code",
	"HandshakeResponse": "IPC handshake frame, never handed to task code",
	"TaskModule":        "shape of the imported task module, consumed by the shim itself",
}

// TestSDKParity_SdkDtsDeclaresShimInterfaces asserts that, for every
// interface in shim.ts other than the shim-internal ones, sdk.d.ts declares
// an interface of the same name with every member (nested namespace keys
// included) the shim declares.
func TestSDKParity_SdkDtsDeclaresShimInterfaces(t *testing.T) {
	shim := stripTSComments(readFile(t, shimPath))
	dts := stripTSComments(readFile(t, sdkDtsPath))
	names := regexp.MustCompile(`\binterface\s+(\w+)\s*\{`).FindAllStringSubmatch(shim, -1)
	seen := map[string]bool{}
	for _, n := range names {
		seen[n[1]] = true
	}
	for name, reason := range shimInternalInterfaces {
		if !seen[name] {
			t.Errorf("skipped interface %s is not declared in shim.ts; drop it", name)
		}
		if reason == "" {
			t.Errorf("skipped interface %s has no reason", name)
		}
	}
	for _, name := range sortedKeys(seen) {
		if _, internal := shimInternalInterfaces[name]; internal {
			continue
		}
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
	if !seen["Dicode"] || !seen["Output"] {
		t.Fatal("shim.ts interface scan missed Dicode or Output; the scanner is broken")
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

// interfaceMembers returns the dotted member paths of `interface <name> {…}`
// in src. Keys inside parameter lists and generic arguments are ignored, so
// only members and nested object-type namespaces are reported.
func interfaceMembers(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	src = stripTSComments(src)
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
