package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

func TestDownloadFailureReasonIsReadableAndHidesDetails(t *testing.T) {
	leaky := "https://user:token@github.com/org/repo at /private/org/projects/x"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unknown commit", fmt.Errorf("%w: commit abc in %s: exit 128", ErrCommitUnresolvable, leaky), "the requested commit could not be found in the repository"},
		{"timeout", fmt.Errorf("git clone %s: %w", leaky, context.DeadlineExceeded), "the repository download timed out"},
		{"other git error", fmt.Errorf("git clone %s: exit status 128", leaky), "the repository could not be downloaded; check that it exists and that the integration can access it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadFailureReason(tc.err); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// recordingConnector captures each statement so writes can be checked without a database.
type recordingConnector struct{ queries *[]string }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return recordingConn(c), nil
}
func (recordingConnector) Driver() driver.Driver { return nil }

type recordingConn struct{ queries *[]string }

func (recordingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (recordingConn) Close() error                        { return nil }
func (recordingConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	*c.queries = append(*c.queries, query)
	return driver.RowsAffected(1), nil
}

func TestMarkAnalysisFailedKeepsTheFirstReasonAndHidesPaths(t *testing.T) {
	var queries []string
	db := bun.NewDB(sql.OpenDB(recordingConnector{&queries}), pgdialect.New())
	defer db.Close()

	markAnalysisFailed(db, uuid.New(), "cannot read /private/org/archive.zip")

	if len(queries) != 1 {
		t.Fatalf("expected one write, got %v", queries)
	}
	if strings.Contains(queries[0], "/private") || !strings.Contains(queries[0], "[path]") {
		t.Errorf("path not hidden: %s", queries[0])
	}
	if !strings.Contains(queries[0], "COALESCE(failure_reason") {
		t.Errorf("an earlier reason could be overwritten: %s", queries[0])
	}
}
