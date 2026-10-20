package internal

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The runtime premises of #344: a Session does not carry over every field of
// the Statement it copies. The next call on a Session result runs
// Statement.clone() (gorm statement.go), which leaves out the fields in
// statementUncloned. So a Session after Attrs or Assign drops them, and one
// after InstanceSet loses its value, whose key holds the Statement's address.
// The analyzer withholds every Session fix for such a value and does not
// advise one (internal/ssa/tracer/state.go).

// TestUnclonedState_StatementFields checks statementCloned and
// statementUncloned (gorm_stub_test.go) at run time: after a clone each
// field of statementCloned still holds the value set before it, and each
// other one is empty (DB: another handle).
func TestUnclonedState_StatementFields(t *testing.T) {
	typ := reflect.TypeFor[gorm.Statement]()
	clones := map[string]func(*gorm.DB) *gorm.DB{
		// What the suggested fix makes: the next call on the Session result.
		"chain after Session": func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{}).Unscoped() },
		"Initialized":         func(q *gorm.DB) *gorm.DB { return q.Session(&gorm.Session{Initialized: true}) },
		"WithContext":         func(q *gorm.DB) *gorm.DB { return q.WithContext(context.Background()) },
	}
	for _, name := range slices.Sorted(maps.Keys(clones)) {
		t.Run(name, func(t *testing.T) {
			db, _ := setupDB(t)
			q := db.Model(&User{}) // clone==0
			stmt := reflect.ValueOf(q.Statement).Elem()
			for f := range typ.Fields() {
				if f.Name != "DB" {
					statementFill(t, statementField(stmt, f.Name))
				}
			}
			clone := reflect.ValueOf(clones[name](q).Statement).Elem()
			for f := range typ.Fields() {
				field := statementField(clone, f.Name)
				_, uncloned := statementUncloned[f.Name]
				switch {
				case f.Name == "DB":
					if field.Pointer() == reflect.ValueOf(q).Pointer() {
						t.Errorf("the clone's DB is the original handle")
					}
				case f.Name == "Context" && name == "WithContext":
					// WithContext sets its own.
				case uncloned && statementHolds(field):
					t.Errorf("Statement.%s survived the clone, but statementUncloned lists it", f.Name)
				case !uncloned && !statementHolds(field):
					t.Errorf("Statement.%s did not survive the clone; move it to statementUncloned", f.Name)
				}
			}
		})
	}
}

// statementField returns the field name of stmt, a gorm.Statement, settable
// even when it is unexported.
func statementField(stmt reflect.Value, name string) reflect.Value {
	f := stmt.FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// statementFill sets f to a value that statementHolds reports.
func statementFill(t *testing.T, f reflect.Value) {
	t.Helper()
	switch addr := f.Addr().Interface().(type) {
	case *sync.Map:
		addr.Store("k", 1)
		return
	case *strings.Builder:
		addr.WriteString("SELECT 1")
		return
	case *reflect.Value:
		*addr = reflect.ValueOf(1)
		return
	case *context.Context:
		*addr = context.WithValue(context.Background(), statementKey{}, 1)
		return
	}
	switch f.Kind() {
	case reflect.Pointer:
		f.Set(reflect.New(f.Type().Elem()))
	case reflect.Slice:
		f.Set(reflect.MakeSlice(f.Type(), 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(f.Type())
		key := reflect.New(f.Type().Key()).Elem()
		if key.Kind() == reflect.String {
			key.SetString("k")
		}
		m.SetMapIndex(key, reflect.New(f.Type().Elem()).Elem())
		f.Set(m)
	case reflect.String:
		f.SetString("x")
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int:
		f.SetInt(1)
	case reflect.Interface:
		if f.IsNil() {
			f.Set(reflect.ValueOf(1))
		}
	default:
		t.Fatalf("cannot fill a Statement field of type %s", f.Type())
	}
}

// statementKey is the context key that statementFill stores.
type statementKey struct{}

// statementHolds reports whether f holds a value: a non-empty map, slice,
// builder or sync.Map, or a non-zero value of any other type.
func statementHolds(f reflect.Value) bool {
	switch v := f.Interface().(type) {
	case sync.Map:
		n := 0
		v.Range(func(any, any) bool { n++; return true })
		return n > 0
	case strings.Builder:
		return v.Len() > 0
	case reflect.Value:
		return v.IsValid()
	}
	switch f.Kind() {
	case reflect.Map, reflect.Slice:
		return f.Len() > 0
	}
	return !f.IsZero()
}

// unclonedStateDB opens a gorm DB on sqlmock whose queries return rows, or
// no row when empty is set.
func unclonedStateDB(t *testing.T, empty bool) *gorm.DB {
	t.Helper()
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("Failed to create sqlmock: %v", err)
	}
	for range 4 {
		rows := sqlmock.NewRows([]string{"id", "name", "active", "age"})
		if !empty {
			rows.AddRow(1, "a", true, 20)
		}
		mock.ExpectQuery(".*").WillReturnRows(rows)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: mockDB, SkipInitializeWithVersion: true}), &gorm.Config{
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("Failed to open gorm: %v", err)
	}
	return db
}

// TestUnclonedState_AssignDropped: the fix suggested before #344 appended a
// Session to `q := db.Model(&User{}).Assign(User{Age: 77})`. Without it the
// found row gets Age 77; with it, the row's own 20.
func TestUnclonedState_AssignDropped(t *testing.T) {
	for _, session := range []bool{false, true} {
		db := unclonedStateDB(t, false)
		q := db.Model(&User{}).Assign(User{Age: 77})
		if session {
			q = q.Session(&gorm.Session{})
		}
		var u User
		q.Where("name = ?", "a").FirstOrInit(&u)
		if want := map[bool]int{false: 77, true: 20}[session]; u.Age != want {
			t.Errorf("session=%v: Age = %d, want %d", session, u.Age, want)
		}
	}
}

// TestUnclonedState_AttrsDropped: with no row found, Attrs initializes Age
// to 30, unless a Session came after it.
func TestUnclonedState_AttrsDropped(t *testing.T) {
	for _, session := range []bool{false, true} {
		db := unclonedStateDB(t, true)
		q := db.Model(&User{}).Attrs(User{Age: 30})
		if session {
			q = q.Session(&gorm.Session{})
		}
		var u User
		q.Where("name = ?", "a").FirstOrInit(&u)
		if want := map[bool]int{false: 30, true: 0}[session]; u.Age != want {
			t.Errorf("session=%v: Age = %d, want %d", session, u.Age, want)
		}
	}
}

// unclonedStateParam is what the parameter-root fix made of a function that
// branches its parameter twice before #344: a Session at the top of the body.
func unclonedStateParam(tx *gorm.DB, session bool) (ages []int) {
	if session {
		tx = tx.Session(&gorm.Session{})
	}
	for _, name := range []string{"a", "b"} {
		var u User
		tx.Where("name = ?", name).FirstOrInit(&u)
		ages = append(ages, u.Age)
	}
	return ages
}

// TestUnclonedState_ParamSessionDropsCallerAssign: the body Session drops an
// Assign that the caller passed in.
func TestUnclonedState_ParamSessionDropsCallerAssign(t *testing.T) {
	for _, session := range []bool{false, true} {
		db := unclonedStateDB(t, false)
		ages := unclonedStateParam(db.Model(&User{}).Assign(User{Age: 77}), session)
		want := map[bool][]int{false: {77, 77}, true: {20, 20}}[session]
		if !slices.Equal(ages, want) {
			t.Errorf("session=%v: ages = %v, want %v", session, ages, want)
		}
	}
}

// TestUnclonedState_InstanceSetLost: InstanceSet keys its value by the
// Statement's address, so the clone after a Session does not find it, though
// Settings is copied. Set's plain key survives.
func TestUnclonedState_InstanceSetLost(t *testing.T) {
	db, _ := setupDB(t)
	q := db.Model(&User{}).InstanceSet("k", 1).Set("plain", 2)
	if _, ok := q.Where("a = 1").InstanceGet("k"); !ok {
		t.Errorf("a chain on the clone==0 value should find the InstanceSet value")
	}
	s := db.Model(&User{}).InstanceSet("k", 1).Set("plain", 2).Session(&gorm.Session{})
	if _, ok := s.Where("a = 1").InstanceGet("k"); ok {
		t.Errorf("a chain on the Session result should not find the InstanceSet value")
	}
	if _, ok := s.Where("a = 1").Get("plain"); !ok {
		t.Errorf("a chain on the Session result should find the Set value")
	}
}
