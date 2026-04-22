package secrets

import (
	"strings"
	"sync"
	"testing"
)

func TestRedactor_NilAndZeroValue(t *testing.T) {
	t.Parallel()

	// nil receiver must be safe and pass the string through.
	var nilR *Redactor
	if got := nilR.RedactString("hello"); got != "hello" {
		t.Errorf("nil receiver RedactString(%q) = %q; want %q", "hello", got, "hello")
	}

	// Zero-value Redactor (no replacer) must be safe and pass-through.
	var zero Redactor
	if got := zero.RedactString("hello"); got != "hello" {
		t.Errorf("zero Redactor RedactString(%q) = %q; want %q", "hello", got, "hello")
	}
}

func TestRedactor_EmptyValueMap(t *testing.T) {
	t.Parallel()
	r := NewRedactor(nil)
	if got := r.RedactString("anything"); got != "anything" {
		t.Errorf("NewRedactor(nil) redacts %q; want pass-through", got)
	}
	r = NewRedactor(map[string]string{})
	if got := r.RedactString("anything"); got != "anything" {
		t.Errorf("NewRedactor(empty) redacts %q; want pass-through", got)
	}
}

func TestRedactor_SingleValue(t *testing.T) {
	t.Parallel()
	r := NewRedactor(map[string]string{"MY_TOKEN": "s3cr3t"})
	got := r.RedactString("got: s3cr3t (end)")
	want := "got: " + RedactionMarker + " (end)"
	if got != want {
		t.Errorf("RedactString = %q; want %q", got, want)
	}
}

func TestRedactor_MultipleOccurrencesSameValue(t *testing.T) {
	t.Parallel()
	r := NewRedactor(map[string]string{"T": "pw"})
	got := r.RedactString("pw and pw again: pw")
	want := RedactionMarker + " and " + RedactionMarker + " again: " + RedactionMarker
	if got != want {
		t.Errorf("RedactString = %q; want %q", got, want)
	}
}

func TestRedactor_OverlappingValuesLongestFirst(t *testing.T) {
	t.Parallel()
	// The short value "foo" is a substring of the long value "foobar".
	// strings.Replacer's trie must prefer the long match at each position,
	// otherwise "foobar" would be redacted as "<REDACTED>bar".
	r := NewRedactor(map[string]string{"short": "foo", "long": "foobar"})
	got := r.RedactString("see foobar please")
	want := "see " + RedactionMarker + " please"
	if got != want {
		t.Errorf("overlap: RedactString = %q; want %q (longest match must win)", got, want)
	}
	// And bare "foo" alone still gets redacted.
	got = r.RedactString("say foo then")
	want = "say " + RedactionMarker + " then"
	if got != want {
		t.Errorf("short-match: RedactString = %q; want %q", got, want)
	}
}

func TestRedactor_EmptyValueIsDropped(t *testing.T) {
	t.Parallel()
	// An empty-string secret would match every position if not dropped —
	// this test pins the pathological case out.
	r := NewRedactor(map[string]string{"EMPTY": "", "T": "token"})
	got := r.RedactString("hello token world")
	want := "hello " + RedactionMarker + " world"
	if got != want {
		t.Errorf("RedactString = %q; want %q", got, want)
	}
	// And a pure-empty-value redactor passes through.
	r = NewRedactor(map[string]string{"ONLY_EMPTY": ""})
	if got := r.RedactString("hello"); got != "hello" {
		t.Errorf("all-empty redactor: RedactString(%q) = %q; want pass-through", "hello", got)
	}
}

func TestRedactor_DuplicateValuesAreDeduped(t *testing.T) {
	t.Parallel()
	// Two keys resolving to the same value must not panic
	// strings.NewReplacer (which disallows duplicate old-strings).
	r := NewRedactor(map[string]string{"A": "same", "B": "same"})
	got := r.RedactString("same stuff")
	want := RedactionMarker + " stuff"
	if got != want {
		t.Errorf("RedactString = %q; want %q", got, want)
	}
}

func TestRedactor_SingleCharValueAccepted(t *testing.T) {
	t.Parallel()
	// One-char secrets are accepted — if a caller stored one, leaking
	// beats noisy output. Pinned explicitly so nobody adds a min-length
	// filter without seeing the intent.
	r := NewRedactor(map[string]string{"CH": "x"})
	got := r.RedactString("axbx")
	want := "a" + RedactionMarker + "b" + RedactionMarker
	if got != want {
		t.Errorf("RedactString = %q; want %q", got, want)
	}
}

func TestRedactor_DoesNotRedactKeyNames(t *testing.T) {
	t.Parallel()
	// A secret registered under key MY_TOKEN must NOT redact the string
	// "MY_TOKEN" in log output — only its value.
	r := NewRedactor(map[string]string{"MY_TOKEN": "actual-secret"})
	got := r.RedactString("env MY_TOKEN not set; used actual-secret instead")
	if !strings.Contains(got, "MY_TOKEN") {
		t.Errorf("RedactString redacted the key name, not just the value: %q", got)
	}
	if strings.Contains(got, "actual-secret") {
		t.Errorf("RedactString failed to redact the value: %q", got)
	}
}

func TestRedactor_ConcurrentRedactString(t *testing.T) {
	t.Parallel()
	r := NewRedactor(map[string]string{"T": "token"})
	var wg sync.WaitGroup
	const goroutines = 16
	const iters = 200
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if got := r.RedactString("hello token"); got != "hello "+RedactionMarker {
					t.Errorf("concurrent RedactString = %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
