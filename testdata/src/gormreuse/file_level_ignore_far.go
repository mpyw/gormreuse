//gormreuse:ignore
//
// Copyright 2026 The gormreuse Authors.
// Licensed under the Apache License, Version 2.0 (the "License").
// This header is deliberately long enough to push the directive more than
// five lines above the package clause. The old fixed 5-line window failed
// to treat such a directive as file-level (#70); it now does.

package internal

import "gorm.io/gorm"

// All violations in this file are suppressed by the far file-level ignore.
func fileLevelIgnoreFar1(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.Count(nil) // Would be VIOLATION, but suppressed by file-level ignore
}

func fileLevelIgnoreFar2(db *gorm.DB) {
	q := db.Where("x = ?", 1)
	q.Find(nil)
	q.First(nil) // Would be VIOLATION, but suppressed
}
