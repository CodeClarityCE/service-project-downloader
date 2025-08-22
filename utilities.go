package main

import (
	"context"

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
		panic(err)
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
		panic(err)
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
		panic(err)
	}

	return *integration_document, nil
}
