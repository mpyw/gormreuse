package purelib

import "gorm.io/gorm"

// ReceiverRepo holds a handle. Its pure methods are checked for what they do with it,
// and another package reads the verdict (#404).
type ReceiverRepo struct{ DB *gorm.DB }

// Changes runs a chain on the handle its receiver holds.
//
//gormreuse:pure
func (r ReceiverRepo) Changes(dst *gorm.DB) { // want Changes:"gormreuse pure pure-changes-receiver"
	r.DB.Where("x").Find(nil)
}

// Reads only reads it.
//
//gormreuse:pure
func (r ReceiverRepo) Reads(dst *gorm.DB) { // want Reads:"gormreuse pure"
	r.DB.Session(&gorm.Session{}).Count(nil)
}
