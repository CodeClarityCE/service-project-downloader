package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/CodeClarityCE/utility-boilerplates"
	types_amqp "github.com/CodeClarityCE/utility-types/amqp"
	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// tableDB answers a select with the row stored for its table, if any, and records every write.
type tableDB struct {
	rows   map[string]map[string]string
	writes *[]string
}

func (f tableDB) Connect(context.Context) (driver.Conn, error) { return tableConn(f), nil }
func (tableDB) Driver() driver.Driver                          { return nil }

type tableConn tableDB

func (tableConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (tableConn) Close() error                        { return nil }
func (c tableConn) Begin() (driver.Tx, error) {
	*c.writes = append(*c.writes, "BEGIN")
	return nil, errors.New("not supported")
}
func (c tableConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	*c.writes = append(*c.writes, query)
	return driver.RowsAffected(1), nil
}
func (c tableConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	for table, row := range c.rows {
		if strings.Contains(query, `FROM "`+table+`"`) {
			return newRowsOf(row), nil
		}
	}
	return newRowsOf(nil), nil
}

type rowsOf struct {
	cols []string
	row  map[string]string
	done bool
}

func newRowsOf(row map[string]string) *rowsOf {
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return &rowsOf{cols: cols, row: row}
}

func (r *rowsOf) Columns() []string { return r.cols }
func (r *rowsOf) Close() error      { return nil }
func (r *rowsOf) Next(dest []driver.Value) error {
	if r.row == nil || r.done {
		return io.EOF
	}
	r.done = true
	for i, c := range r.cols {
		dest[i] = []byte(r.row[c])
	}
	return nil
}

// downloadFor delivers one message for a FILE project with an uploaded archive.
// analysis is the stored analysis row, or nil when it was deleted.
func downloadFor(t *testing.T, analysis map[string]string, analysisId, projectId, orgId uuid.UUID) (downloadPath string, writes []string, logs string) {
	t.Helper()
	downloadPath = t.TempDir()
	t.Setenv("DOWNLOAD_PATH", downloadPath)
	uploadDir := filepath.Join(downloadPath, uuid.NewString(), projectId.String())
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	upload, err := os.Create(filepath.Join(uploadDir, "upload.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if err := zip.NewWriter(upload).Close(); err != nil {
		t.Fatal(err)
	}
	upload.Close()

	rows := map[string]map[string]string{
		"project": {"id": projectId.String(), "type": "FILE"},
	}
	if analysis != nil {
		rows["analysis"] = analysis
	}
	db := bun.NewDB(sql.OpenDB(tableDB{rows: rows, writes: &writes}), pgdialect.New())
	defer db.Close()
	var out bytes.Buffer
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(&out)
	service := &boilerplates.ServiceBase{
		DB:          &boilerplates.ServiceDatabases{CodeClarity: db},
		Logger:      logger,
		ServiceName: "downloader",
	}
	body, _ := json.Marshal(types_amqp.DispatcherDownloaderMessage{
		AnalysisId:     analysisId,
		ProjectId:      projectId,
		OrganizationId: orgId,
	})

	dispatch("dispatcher_downloader", amqp.Delivery{Body: body}, service)
	return downloadPath, writes, out.String()
}

func assertSkipped(t *testing.T, downloadPath string, orgId, analysisId uuid.UUID, writes []string, logs string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(downloadPath, orgId.String())); !os.IsNotExist(err) {
		t.Errorf("a project folder was created for a skipped analysis")
	}
	if len(writes) != 0 {
		t.Errorf("expected no writes, got %v", writes)
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one log line, got %q", logs)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", lines[0])
	}
	if entry["level"] != "info" || entry["analysis_id"] != analysisId.String() {
		t.Errorf("expected an info line naming %s, got %v", analysisId, entry)
	}
}

func TestDownloaderSkipsADeletedAnalysis(t *testing.T) {
	analysisId, projectId, orgId := uuid.New(), uuid.New(), uuid.New()
	path, writes, logs := downloadFor(t, nil, analysisId, projectId, orgId)
	assertSkipped(t, path, orgId, analysisId, writes, logs)
}

func TestDownloaderSkipsACancelledAnalysis(t *testing.T) {
	analysisId, projectId, orgId := uuid.New(), uuid.New(), uuid.New()
	path, writes, logs := downloadFor(t, map[string]string{
		"id":        analysisId.String(),
		"projectId": projectId.String(),
		"status":    string(codeclarity.CANCELLED),
	}, analysisId, projectId, orgId)
	assertSkipped(t, path, orgId, analysisId, writes, logs)
}
