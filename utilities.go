package main

import (
	"context"
	"log"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// getAnalysis retrieves an analysis from the database based on the provided analysisID.
// It returns the retrieved analysis and an error if any occurred.
func getAnalysis(db *bun.DB, analysisID uuid.UUID) (codeclarity.Analysis, error) {

	analysis_document := &codeclarity.Analysis{
		Id: analysisID,
	}
	ctx := context.Background()
	err := db.NewSelect().Model(analysis_document).WherePK().Scan(ctx)
	if err != nil {
		return codeclarity.Analysis{}, err
	}

	return *analysis_document, nil
}

// getProject retrieves a project from the database based on the given projectID.
// It returns the project document and an error if any occurred.
func getProject(db *bun.DB, projectID uuid.UUID) (codeclarity.Project, error) {

	project_document := &codeclarity.Project{
		Id: projectID,
	}
	ctx := context.Background()
	err := db.NewSelect().Model(project_document).WherePK().Scan(ctx)
	if err != nil {
		return codeclarity.Project{}, err
	}

	return *project_document, nil
}

// getIntegration retrieves an integration from the database based on the provided integrationID.
// It returns the retrieved integration and an error if any occurred.
func getIntegration(db *bun.DB, integrationID uuid.UUID) (codeclarity.Integration, error) {

	integration_document := &codeclarity.Integration{
		Id: integrationID,
	}
	ctx := context.Background()
	err := db.NewSelect().Model(integration_document).WherePK().Scan(ctx)
	if err != nil {
		return codeclarity.Integration{}, err
	}

	return *integration_document, nil
}

// failureReasonMaxLen matches the API's varchar(500) failure_reason column;
// reasons are truncated so an arbitrarily long git error cannot overflow it.
const failureReasonMaxLen = 500

// markAnalysisFailed sets a non-terminal analysis to FAILURE and persists why
// (failure_reason) in the same UPDATE, so callers polling the analysis can
// distinguish e.g. unresolvable commits from crashes. A download error is
// terminal — re-running it would fail again — so marking it stops the dispatcher's
// reaper from re-driving it forever (the recovery loop is bounded by this write).
// The status guard keeps it idempotent and avoids clobbering an already-terminal
// row. Failures are logged, not propagated: the message is dropped either way.
func markAnalysisFailed(db *bun.DB, analysisID uuid.UUID, reason error) {
	msg := reason.Error()
	// Truncate by runes, not bytes: varchar(500) counts characters, and a
	// byte-level cut could split a multi-byte character into invalid UTF-8.
	if r := []rune(msg); len(r) > failureReasonMaxLen {
		msg = string(r[:failureReasonMaxLen])
	}
	ctx := context.Background()
	res, err := db.NewUpdate().
		Model((*codeclarity.Analysis)(nil)).
		Set("status = ?", codeclarity.FAILURE).
		Set("failure_reason = ?", msg).
		Where("id = ?", analysisID).
		Where("status IN (?)", bun.In([]string{
			string(codeclarity.STARTED),
			string(codeclarity.ONGOING),
		})).
		Exec(ctx)
	if err != nil {
		log.Printf("[downloader] failed to mark analysis %s as FAILURE: %v", analysisID, err)
		return
	}
	// The dispatcher can mark the analysis FAILURE first (its own failure path
	// races this one); the guarded update above then matches nothing and the
	// reason would be lost. Backfill the reason alone — never resurrect or
	// overwrite a row that already carries one.
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := db.NewUpdate().
			Model((*codeclarity.Analysis)(nil)).
			Set("failure_reason = ?", msg).
			Where("id = ?", analysisID).
			Where("status = ?", codeclarity.FAILURE).
			Where("failure_reason IS NULL").
			Exec(ctx); err != nil {
			log.Printf("[downloader] failed to backfill failure_reason for %s: %v", analysisID, err)
		}
	}
}
