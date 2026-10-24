// Package gorm is a stub for testing purposes.
package gorm

import (
	"context"
	"strings"
)

// DB is the main database struct.
type DB struct {
	*Config
	Error        error
	RowsAffected int64
	Statement    *Statement
}

// Config is the configuration that every handle of a connection shares. The
// stub keeps one of its fields and none of its methods.
type Config struct {
	SkipDefaultTransaction bool
}

// Statement holds the clauses of a query.
type Statement struct {
	Table    string
	DB       *DB
	Context  context.Context
	Distinct bool
	Selects  []string
	// Clauses holds clause.Clause values in gorm. gorm.go imports no clause
	// package, so the stub keeps the map only.
	Clauses  map[string]interface{}
	Preloads map[string][]interface{}
	SQL      strings.Builder
}

// AddClause adds a clause to the Statement in place.
func (stmt *Statement) AddClause(v interface{}) {}

// Parse reads the schema of value. It reads only the configuration of
// stmt.DB.
func (stmt *Statement) Parse(value interface{}) error { return nil }

// Session configuration
type Session struct {
	DryRun                   bool
	PrepareStmt              bool
	NewDB                    bool
	Initialized              bool
	SkipHooks                bool
	SkipDefaultTransaction   bool
	DisableNestedTransaction bool
	AllowGlobalUpdate        bool
	FullSaveAssociations     bool
	PropagateUnscoped        bool
	QueryFields              bool
	CreateBatchSize          int
	Context                  context.Context
}

// =============================================================================
// Safe Methods - Return new immutable instance
// =============================================================================

// Session returns a new DB with session configuration.
func (db *DB) Session(config *Session) *DB { return db }

// WithContext returns a new DB with context.
func (db *DB) WithContext(ctx context.Context) *DB { return db }

// =============================================================================
// DB Init Methods - Create new instance
// =============================================================================

// Open opens a database connection.
func Open(dialector interface{}, opts ...interface{}) (*DB, error) { return nil, nil }

// Debug starts debug mode.
func (db *DB) Debug() *DB { return db }

// =============================================================================
// Chain Methods - Modify internal state
// =============================================================================

// Where adds conditions.
func (db *DB) Where(query interface{}, args ...interface{}) *DB { return db }

// Or adds OR conditions.
func (db *DB) Or(query interface{}, args ...interface{}) *DB { return db }

// Not adds NOT conditions.
func (db *DB) Not(query interface{}, args ...interface{}) *DB { return db }

// Select specifies fields to retrieve.
func (db *DB) Select(query interface{}, args ...interface{}) *DB { return db }

// Omit specifies fields to omit.
func (db *DB) Omit(columns ...string) *DB { return db }

// Joins specifies join conditions.
func (db *DB) Joins(query string, args ...interface{}) *DB { return db }

// Group specifies group by fields.
func (db *DB) Group(name string) *DB { return db }

// Having specifies having conditions.
func (db *DB) Having(query interface{}, args ...interface{}) *DB { return db }

// Order specifies order fields.
func (db *DB) Order(value interface{}) *DB { return db }

// Limit specifies limit.
func (db *DB) Limit(limit int) *DB { return db }

// Offset specifies offset.
func (db *DB) Offset(offset int) *DB { return db }

// Scopes applies scopes.
func (db *DB) Scopes(funcs ...func(*DB) *DB) *DB { return db }

// Preload preloads associations.
func (db *DB) Preload(query string, args ...interface{}) *DB { return db }

// Distinct selects distinct values.
func (db *DB) Distinct(args ...interface{}) *DB { return db }

// Unscoped disables soft delete.
func (db *DB) Unscoped() *DB { return db }

// Table specifies table name.
func (db *DB) Table(name string, args ...interface{}) *DB { return db }

// Model specifies model.
func (db *DB) Model(value interface{}) *DB { return db }

// Clauses adds clauses.
func (db *DB) Clauses(conds ...interface{}) *DB { return db }

// Assign specifies values to assign.
func (db *DB) Assign(attrs ...interface{}) *DB { return db }

// Attrs specifies attributes.
func (db *DB) Attrs(attrs ...interface{}) *DB { return db }

// InnerJoins specifies inner join conditions.
func (db *DB) InnerJoins(query string, args ...interface{}) *DB { return db }

// =============================================================================
// Finisher Methods - Execute query (also Chain Methods for our purposes)
// =============================================================================

// Find finds records.
func (db *DB) Find(dest interface{}, conds ...interface{}) *DB { return db }

// First finds first record.
func (db *DB) First(dest interface{}, conds ...interface{}) *DB { return db }

// Last finds last record.
func (db *DB) Last(dest interface{}, conds ...interface{}) *DB { return db }

// Take finds one record.
func (db *DB) Take(dest interface{}, conds ...interface{}) *DB { return db }

// Create creates record.
func (db *DB) Create(value interface{}) *DB { return db }

// Save saves record.
func (db *DB) Save(value interface{}) *DB { return db }

// Update updates column.
func (db *DB) Update(column string, value interface{}) *DB { return db }

// Updates updates columns.
func (db *DB) Updates(values interface{}) *DB { return db }

// Delete deletes record.
func (db *DB) Delete(value interface{}, conds ...interface{}) *DB { return db }

// Count gets count.
func (db *DB) Count(count *int64) *DB { return db }

// Scan scans results.
func (db *DB) Scan(dest interface{}) *DB { return db }

// Row returns row.
func (db *DB) Row() interface{} { return nil }

// Rows returns rows.
func (db *DB) Rows() (interface{}, error) { return nil, nil }

// Pluck plucks column.
func (db *DB) Pluck(column string, dest interface{}) *DB { return db }

// Exec executes raw SQL.
func (db *DB) Exec(sql string, values ...interface{}) *DB { return db }

// Raw executes raw query.
func (db *DB) Raw(sql string, values ...interface{}) *DB { return db }

// FirstOrCreate finds first or creates.
func (db *DB) FirstOrCreate(dest interface{}, conds ...interface{}) *DB { return db }

// FirstOrInit finds first or initializes.
func (db *DB) FirstOrInit(dest interface{}, conds ...interface{}) *DB { return db }

// =============================================================================
// Transaction Methods - DB Init Methods
// =============================================================================

// Begin begins transaction.
func (db *DB) Begin(opts ...interface{}) *DB { return db }

// Commit commits transaction.
func (db *DB) Commit() *DB { return db }

// Rollback rollbacks transaction.
func (db *DB) Rollback() *DB { return db }

// Transaction executes in transaction.
func (db *DB) Transaction(fc func(tx *DB) error, opts ...interface{}) error { return nil }

// Connection executes with a dedicated connection; fc receives a clone==0
// handle (db.getInstance()), not a fresh one.
func (db *DB) Connection(fc func(tx *DB) error) error { return nil }

// FindInBatches finds records in batches; fc receives a fresh handle per batch.
func (db *DB) FindInBatches(dest interface{}, batchSize int, fc func(tx *DB, batch int) error) *DB {
	return db
}

// SavePoint creates save point.
func (db *DB) SavePoint(name string) *DB { return db }

// RollbackTo rollbacks to save point.
func (db *DB) RollbackTo(name string) *DB { return db }

// =============================================================================
// Methods That Return No *DB, and More Finishers (#200)
// =============================================================================

// CreateInBatches creates records in batches.
func (db *DB) CreateInBatches(value interface{}, batchSize int) *DB { return db }

// UpdateColumn updates a column without hooks.
func (db *DB) UpdateColumn(column string, value interface{}) *DB { return db }

// UpdateColumns updates columns without hooks.
func (db *DB) UpdateColumns(values interface{}) *DB { return db }

// AddError adds an error to db.
func (db *DB) AddError(err error) error { return err }

// DB returns the underlying *sql.DB.
func (db *DB) DB() (interface{}, error) { return nil, nil }

// Get reads a value from Statement.Settings.
func (db *DB) Get(key string) (interface{}, bool) { return nil, false }

// Set stores a value in Statement.Settings, which a clone copies.
func (db *DB) Set(key string, value interface{}) *DB { return db }

// InstanceSet stores a value under a key that holds the Statement's
// address, so a clone of the Statement does not find it.
func (db *DB) InstanceSet(key string, value interface{}) *DB { return db }

// InstanceGet reads a value that InstanceSet stored for this Statement.
func (db *DB) InstanceGet(key string) (interface{}, bool) { return nil, false }

// Callbacks is the callback manager.
type Callbacks struct{}

// Callback returns the callback manager.
func (db *DB) Callback() *Callbacks { return &Callbacks{} }

// ToSQL builds the SQL of queryFn run on a DryRun copy of db.
func (db *DB) ToSQL(queryFn func(tx *DB) *DB) string { return "" }

// Association is a model's association.
type Association struct {
	Error error
}

// Association starts association mode.
func (db *DB) Association(column string) *Association { return &Association{} }

// Migrator migrates the schema.
type Migrator interface {
	AutoMigrate(dst ...interface{}) error
}

// Migrator returns a migrator.
func (db *DB) Migrator() Migrator { return nil }

// AutoMigrate migrates the schema of dst.
func (db *DB) AutoMigrate(dst ...interface{}) error { return nil }
