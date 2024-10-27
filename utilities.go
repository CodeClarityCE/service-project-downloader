package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// getAnalysis retrieves an analysis from the database based on the provided analysisID.
// It returns the retrieved analysis and an error if any occurred.
func getAnalysis(analysisID uuid.UUID) (codeclarity.Analysis, error) {
	host := os.Getenv("PG_DB_HOST")
	if host == "" {
		log.Printf("PG_DB_HOST is not set")
		return codeclarity.Analysis{}, fmt.Errorf("PG_DB_HOST is not set")
	}
	port := os.Getenv("PG_DB_PORT")
	if port == "" {
		log.Printf("PG_DB_PORT is not set")
		return codeclarity.Analysis{}, fmt.Errorf("PG_DB_PORT is not set")
	}
	user := os.Getenv("PG_DB_USER")
	if user == "" {
		log.Printf("PG_DB_USER is not set")
		return codeclarity.Analysis{}, fmt.Errorf("PG_DB_USER is not set")
	}
	password := os.Getenv("PG_DB_PASSWORD")
	if password == "" {
		log.Printf("PG_DB_PASSWORD is not set")
		return codeclarity.Analysis{}, fmt.Errorf("PG_DB_PASSWORD is not set")
	}
	name := os.Getenv("PG_DB_NAME")
	if name == "" {
		log.Printf("PG_DB_NAME is not set")
		return codeclarity.Analysis{}, fmt.Errorf("PG_DB_NAME is not set")
	}
	dsn := "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + name + "?sslmode=disable"
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithTimeout(50*time.Second)))
	db := bun.NewDB(sqldb, pgdialect.New())
	defer db.Close()

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
func getProject(projectID uuid.UUID) (codeclarity.Project, error) {
	host := os.Getenv("PG_DB_HOST")
	if host == "" {
		log.Printf("PG_DB_HOST is not set")
		return codeclarity.Project{}, fmt.Errorf("PG_DB_HOST is not set")
	}
	port := os.Getenv("PG_DB_PORT")
	if port == "" {
		log.Printf("PG_DB_PORT is not set")
		return codeclarity.Project{}, fmt.Errorf("PG_DB_PORT is not set")
	}
	user := os.Getenv("PG_DB_USER")
	if user == "" {
		log.Printf("PG_DB_USER is not set")
		return codeclarity.Project{}, fmt.Errorf("PG_DB_USER is not set")
	}
	password := os.Getenv("PG_DB_PASSWORD")
	if password == "" {
		log.Printf("PG_DB_PASSWORD is not set")
		return codeclarity.Project{}, fmt.Errorf("PG_DB_PASSWORD is not set")
	}
	name := os.Getenv("PG_DB_NAME")
	if name == "" {
		log.Printf("PG_DB_NAME is not set")
		return codeclarity.Project{}, fmt.Errorf("PG_DB_NAME is not set")
	}

	dsn := "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + name + "?sslmode=disable"
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithTimeout(50*time.Second)))
	db := bun.NewDB(sqldb, pgdialect.New())
	defer db.Close()

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
func getIntegration(integrationID uuid.UUID) (codeclarity.Integration, error) {
	host := os.Getenv("PG_DB_HOST")
	if host == "" {
		log.Printf("PG_DB_HOST is not set")
		return codeclarity.Integration{}, fmt.Errorf("PG_DB_HOST is not set")
	}
	port := os.Getenv("PG_DB_PORT")
	if port == "" {
		log.Printf("PG_DB_PORT is not set")
		return codeclarity.Integration{}, fmt.Errorf("PG_DB_PORT is not set")
	}
	user := os.Getenv("PG_DB_USER")
	if user == "" {
		log.Printf("PG_DB_USER is not set")
		return codeclarity.Integration{}, fmt.Errorf("PG_DB_USER is not set")
	}
	password := os.Getenv("PG_DB_PASSWORD")
	if password == "" {
		log.Printf("PG_DB_PASSWORD is not set")
		return codeclarity.Integration{}, fmt.Errorf("PG_DB_PASSWORD is not set")
	}
	name := os.Getenv("PG_DB_NAME")
	if name == "" {
		log.Printf("PG_DB_NAME is not set")
		return codeclarity.Integration{}, fmt.Errorf("PG_DB_NAME is not set")
	}

	dsn := "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + name + "?sslmode=disable"
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithTimeout(50*time.Second)))
	db := bun.NewDB(sqldb, pgdialect.New())
	defer db.Close()

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
