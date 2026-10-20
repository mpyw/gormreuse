package before

import "gorm.io/gorm"

// errorPropagationReport runs a query that shows err was returned.
func errorPropagationReport(db *gorm.DB, err error) {
	if err != nil {
		db.Model(&User{}).Where("failed = ?", true).Find(&[]User{})
	}
}

// errorReturned discards the result of a query that fails, and returns the
// Error of a later query on the same mutable root. gorm skips the later
// query, since q.Error is set, and returns the first query's error. A
// Session at q would run it and return nil, so no fix is offered (#372).
func errorReturned(db *gorm.DB) error {
	q := db.Model(&User{})
	q.Find(new(int)) // fails: it scans a row of several columns into an int
	return q.Where("name = ?", "x").Find(&[]User{}).Error
}

// ErrorReturned reports the error that errorReturned returns.
func ErrorReturned(db *gorm.DB) {
	errorPropagationReport(db, errorReturned(db))
}

// ErrorReturnedParam is errorReturned on a parameter, whose fix would be a
// Session at the top of the body (#372).
func ErrorReturnedParam(db *gorm.DB) {
	errorPropagationReport(db, errorReturnedFromParam(db.Model(&User{})))
}

// errorReturnedFromParam is errorReturned with the root as its parameter. It
// gets a mutable value, so its fix would be the body Session.
func errorReturnedFromParam(q *gorm.DB) error {
	q.Find(new(int))
	return q.Where("name = ?", "x").Find(&[]User{}).Error
}

// errorChecked returns the first query's error before the second query
// runs, so the root gets a Session.
func errorChecked(db *gorm.DB) error {
	q := db.Model(&User{})
	if err := q.Where("name = ?", "x").Find(&[]User{}).Error; err != nil {
		return err
	}
	return q.Where("name = ?", "y").Find(&[]User{}).Error
}

// ErrorChecked reports the error that errorChecked returns.
func ErrorChecked(db *gorm.DB) {
	errorPropagationReport(db, errorChecked(db))
}
