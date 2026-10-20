// Package before holds reuse bugs whose suggested fixes the E2E tests run
// against real GORM (#199). Package after holds its files with every fix that
// gormreuse suggests applied. TestGoldenFilesE2E in the main module keeps it
// up to date.
//
// Each function runs its queries on db and returns nothing: the tests read the
// SQL that db emits. A topic's fixtures go in a file of their own, and its
// test file registers its cases with registerFixsqlCases.
package before
