package externalpure

import "purelib"

// Functions of purelib inferred immutable-return export the fact, so their
// results are immutable here too (#261).
func inferredAcrossPackages() {
	db := purelib.InferredSession()
	db.Find(nil)
	db.Find(nil) // OK

	q, err := purelib.InferredOpen()
	if err != nil {
		return
	}
	q.Find(nil)
	q.Find(nil) // OK

	c := purelib.InferredChain()
	c.Find(nil)
	c.Find(nil) // OK
}

// A function inferred only through a broken contract exports no fact.
func inferredThroughBrokenContract() {
	v := purelib.ViaBadImmutableReturn()
	v.Find(nil)
	v.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`

	w := purelib.ViaViaBadImmutableReturn()
	w.Find(nil)
	w.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`

	n := purelib.NotInferred()
	n.Find(nil)
	n.Find(nil) // want `\*gorm\.DB reused: second branch from mutable root`
}
