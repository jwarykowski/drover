package exec

import (
	"fmt"
	"strings"
)

// renderAll fills {{key}} placeholders in each argv element from vals, rejecting
// any value carrying a newline or NUL.
//
// This is the untrusted path: vals here come from event data, which drover did
// not author. A newline could smuggle an extra line into anything that later
// parses the result line-wise, and a NUL truncates the string at the execve
// boundary — so both fail closed rather than being escaped.
func renderAll(tmpl []string, vals map[string]string) ([]string, error) {
	out := make([]string, len(tmpl))
	for i, e := range tmpl {
		r, err := render(e, vals, false)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// renderArgv is renderAll for a trusted template with a drover-built prompt.
//
// The prompt is multi-line by construction, so the newline rule cannot apply
// here. That is safe for a different reason than escaping: each element becomes
// one execve argument directly, with no shell and no re-parsing, so a newline
// inside a value cannot become a second argument or a second command. NUL is
// still rejected — it would silently truncate the argument.
func renderArgv(tmpl []string, vals map[string]string) ([]string, error) {
	out := make([]string, len(tmpl))
	for i, e := range tmpl {
		r, err := render(e, vals, true)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// renderLoose fills {{key}} from vals for the prompt BODY (an action's Do), so a
// prompt can weave event fields inline: `Review {{url}} in {{repo}}`.
//
// Unlike render it never errors: an unknown key renders empty (a prompt may name
// an optional field a given event lacks) and an unterminated `{{` is left
// literal. Newlines are fine — the body is multi-line and, unlike argv, is only
// ever fenced as data in the prompt, never re-parsed. NUL is stripped.
func renderLoose(s string, vals map[string]string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s) // unterminated: leave the rest literal
			break
		}
		key := strings.TrimSpace(s[i+2 : i+j])
		b.WriteString(s[:i])
		b.WriteString(strings.ReplaceAll(vals[key], "\x00", ""))
		s = s[i+j+2:]
	}
	return b.String()
}

// render substitutes {{key}} tokens with vals[key]. An unknown placeholder is
// rejected: a template naming a key the event never carries is a config error,
// and silently emptying it would run the agent somewhere unintended.
func render(s string, vals map[string]string, allowNewline bool) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("unterminated placeholder in %q", s)
		}
		key := strings.TrimSpace(s[i+2 : i+j])
		val, ok := vals[key]
		if !ok {
			return "", fmt.Errorf("no value for placeholder {{%s}}", key)
		}
		bad := "\n\x00"
		if allowNewline {
			bad = "\x00"
		}
		if strings.ContainsAny(val, bad) {
			return "", fmt.Errorf("value for %q contains a newline or NUL", key)
		}
		b.WriteString(s[:i])
		b.WriteString(val)
		s = s[i+j+2:]
	}
	return b.String(), nil
}
