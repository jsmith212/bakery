package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Output rendering.
//
// Human-readable by default, --json where it is cheap -- which is everywhere the
// server already hands us a JSON document, i.e. everywhere. The human form is a
// tabwriter table with lowercase headers; the JSON form is the server's own
// response, re-encoded, so `bakery org list --json | jq` sees exactly the
// documented wire shape and not a CLI-invented projection of it.
//
// Voice: sentence case, terse, technical. No emoji. No exclamation points.

// renderer decides between the two.
type renderer struct {
	out  io.Writer
	json bool
}

// value prints v as JSON, or calls human.
func (r renderer) value(v any, human func(io.Writer)) error {
	if !r.json {
		human(r.out)

		return nil
	}

	enc := json.NewEncoder(r.out)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode the output: %w", err)
	}

	return nil
}

// list prints a collection as JSON, or as a table.
//
// The JSON form keeps the API's {"items": [...]} envelope rather than emitting a
// bare array: a script that pipes this to jq should be reading the same document
// the API returned, so that when the envelope grows a next_cursor the script does
// not have to change shape.
func (r renderer) list(items any, headers []string, rows [][]string, empty string) error {
	if r.json {
		return r.value(map[string]any{"items": items}, nil)
	}

	if len(rows) == 0 {
		fmt.Fprintln(r.out, empty)

		return nil
	}

	table(r.out, headers, rows)

	return nil
}

// table writes an aligned, header-first table.
func table(out io.Writer, headers []string, rows [][]string) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, strings.Join(headers, "\t"))

	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}

	// tabwriter buffers everything until Flush; the error it returns is the
	// underlying writer's, and there is nowhere useful to send a failure to write
	// to stdout.
	_ = w.Flush()
}

// fields writes a two-column key/value block: the single-object counterpart of a
// table.
func fields(out io.Writer, pairs [][2]string) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	for _, p := range pairs {
		fmt.Fprintf(w, "%s\t%s\n", p[0], p[1])
	}

	_ = w.Flush()
}

// dash renders an empty string as "-", so a column never collapses and a missing
// value is visibly missing rather than invisibly absent.
func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// ts renders a nullable timestamp. RFC 3339, never "2 minutes ago": the console
// humanizes, the CLI does not, because a shell pipeline cannot parse English.
func ts(t *time.Time) string {
	if t == nil {
		return "-"
	}

	return t.UTC().Format(time.RFC3339)
}
