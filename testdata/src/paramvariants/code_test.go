package paramvariants

import "gorm.io/gorm"

func testCode(db *gorm.DB) {
	helper(db.Where("x"))
}
