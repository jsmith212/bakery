package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/config"
)

// `bakery gc run` and `bakery gc list` -- HTTP client verbs over POST /gc/run and
// GET /gc/runs[, /{id}] (spec §9.10), the same shape as `sstate push`: this file
// walks nothing and touches no database, it only calls the API and renders what
// comes back.

// gcPollInterval is how often --wait polls GET /gc/runs/{id}. A sweep runs on the
// order of minutes to hours (the shipped six-hour interval, spec §9.8), so this is
// deliberately coarser than login.go's device-grant poll, which is waiting on a
// human clicking a button in the next few seconds, not on a scan of millions of
// rows.
const gcPollInterval = 2 * time.Second

// gcRun triggers a sweep. The server answers as soon as it has a run id -- NEVER
// once the sweep finishes (spec §9.10) -- so by default this prints that id and
// returns immediately. --wait opts a human into blocking here instead, polling
// until the run leaves 'running'.
func gcRun(ctx context.Context, c *Client, r renderer, cmd config.GCRunCmd) error {
	triggered, err := c.TriggerGCRun(ctx, cmd.DryRun)
	if err != nil {
		return err
	}

	if !cmd.Wait {
		return r.value(triggered, func(out io.Writer) {
			label := "started"
			if cmd.DryRun {
				label = "started (dry run: nothing will be deleted)"
			}

			fmt.Fprintf(out, "gc run %d %s\n", triggered.ID, label)
		})
	}

	run, err := waitForGCRun(ctx, c, triggered.ID)
	if err != nil {
		return err
	}

	return r.value(run, func(out io.Writer) {
		printGCRun(out, run)
	})
}

// waitForGCRun polls until the run leaves 'running'. There is no deadline of its
// own: --wait blocks for as long as the sweep takes, and ctrl-c cancels ctx and
// unblocks it, exactly like the device-grant poll in login.go.
func waitForGCRun(ctx context.Context, c *Client, id int64) (api.GCRun, error) {
	for {
		run, err := c.GetGCRun(ctx, id)
		if err != nil {
			return api.GCRun{}, err
		}

		if run.Status != "running" {
			return run, nil
		}

		if err := c.sleep(ctx, gcPollInterval); err != nil {
			return api.GCRun{}, fmt.Errorf("wait for gc run %d: %w", id, err)
		}
	}
}

// gcList lists recent runs, most recent first. The API's next_cursor rides along
// in --json output (r.value passes the whole api.GCRunList through); the CLI does
// not offer a flag to page through it today -- an operator watching runs is
// looking at the newest ones, and a script that needs every row reads --json and
// paginates itself.
func gcList(ctx context.Context, c *Client, r renderer, cmd config.GCListCmd) error {
	runs, err := c.ListGCRuns(ctx, cmd.Status, cmd.Limit)
	if err != nil {
		return err
	}

	return r.value(runs, func(out io.Writer) {
		if len(runs.Items) == 0 {
			fmt.Fprintln(out, "no gc runs")

			return
		}

		rows := make([][]string, 0, len(runs.Items))
		for _, run := range runs.Items {
			rows = append(rows, []string{
				strconv.FormatInt(run.ID, 10), run.Status, run.Trigger, yesNo(run.DryRun),
				run.StartedAt.UTC().Format(time.RFC3339), ts(run.FinishedAt),
				strconv.FormatInt(run.ObjectsDeleted, 10), humanBytes(run.BytesReclaimed),
			})
		}

		table(out, []string{
			"id", "status", "trigger", "dry run", "started", "finished",
			"objects deleted", "bytes reclaimed",
		}, rows)
	})
}

// printGCRun is the single-run detail view `gc run --wait` and a future `gc show`
// would share.
func printGCRun(out io.Writer, run api.GCRun) {
	fields(out, [][2]string{
		{"id", strconv.FormatInt(run.ID, 10)},
		{"status", run.Status},
		{"trigger", run.Trigger},
		{"dry run", yesNo(run.DryRun)},
		{"started", run.StartedAt.UTC().Format(time.RFC3339)},
		{"finished", ts(run.FinishedAt)},
		{"objects deleted", strconv.FormatInt(run.ObjectsDeleted, 10)},
		{"hashserv rows deleted", strconv.FormatInt(run.HashservRowsDeleted, 10)},
		{"blobs marked", strconv.FormatInt(run.BlobsMarked, 10)},
		{"blobs deleted", strconv.FormatInt(run.BlobsDeleted, 10)},
		{"bytes reclaimed", humanBytes(run.BytesReclaimed)},
	})

	if run.Error != "" {
		fmt.Fprintf(out, "error: %s\n", run.Error)
	}
}

// yesNo renders a bool the way dash() renders an empty string -- a column that
// never collapses.
func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "-"
}
