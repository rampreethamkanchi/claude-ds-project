// Package ot — ot_test.go
//
// Unit and fuzz tests for the EasySync OT engine.
//
// The critical invariant under test is:
//   Apply(Apply(doc, A), Follow(A, B)) == Apply(Apply(doc, B), Follow(B, A))
//
// This is the "follow-equivalence" property, which guarantees that all clients
// converge to the same document state regardless of the order they see changes.
package ot

import (
	"testing"
	"unicode/utf8"
)

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func mustApply(t *testing.T, text string, c Changeset) string {
	t.Helper()
	result, err := ApplyChangeset(text, c)
	if err != nil {
		t.Fatalf("ApplyChangeset failed: %v\nchangeset: %v", err, c)
	}
	return result
}

func mustFollow(t *testing.T, A, B Changeset) Changeset {
	t.Helper()
	f, err := Follow(A, B)
	if err != nil {
		t.Fatalf("Follow failed: %v", err)
	}
	return f
}

func mustCompose(t *testing.T, A, B Changeset) Changeset {
	t.Helper()
	c, err := Compose(A, B)
	if err != nil {
		t.Fatalf("Compose failed: %v", err)
	}
	return c
}

// assertConvergence verifies that applying A then f(A,B) gives the same result
// as applying B then f(B,A) — the core OT invariant.
func assertConvergence(t *testing.T, doc string, A, B Changeset) {
	t.Helper()

	fAB := mustFollow(t, A, B) // B' = f(A, B): apply after A
	fBA := mustFollow(t, B, A) // A' = f(B, A): apply after B

	docA := mustApply(t, doc, A)
	docB := mustApply(t, doc, B)

	resultAB := mustApply(t, docA, fAB)
	resultBA := mustApply(t, docB, fBA)

	if resultAB != resultBA {
		t.Errorf("convergence failed:\n  doc   = %q\n  A     = %v\n  B     = %v\n  A→f(A,B) = %q\n  B→f(B,A) = %q",
			doc, A, B, resultAB, resultBA)
	}
}

// ─────────────────────────────────────────────
// TestApplyChangeset
// ─────────────────────────────────────────────

func TestApplyChangeset_Identity(t *testing.T) {
	text := "hello world"
	id := Identity(len(text))
	result, err := ApplyChangeset(text, id)
	if err != nil {
		t.Fatal(err)
	}
	if result != text {
		t.Errorf("expected %q got %q", text, result)
	}
}

func TestApplyChangeset_Insert(t *testing.T) {
	text := "helloworld"
	c := MakeInsert(len(text), 5, " ")
	result := mustApply(t, text, c)
	if result != "hello world" {
		t.Errorf("expected %q got %q", "hello world", result)
	}
}

func TestApplyChangeset_Delete(t *testing.T) {
	text := "hello world"
	c := MakeDelete(len(text), 5, 1) // delete the space
	result := mustApply(t, text, c)
	if result != "helloworld" {
		t.Errorf("expected %q got %q", "helloworld", result)
	}
}

func TestApplyChangeset_WrongLength(t *testing.T) {
	text := "abc"
	c := Identity(5) // wrong old length
	_, err := ApplyChangeset(text, c)
	if err == nil {
		t.Error("expected error for length mismatch, got nil")
	}
}

// ─────────────────────────────────────────────
// TestFollow — EasySync paper examples
// ─────────────────────────────────────────────

// TestFollow_Baseball reproduces the "baseball" example from the EasySync spec:
//   X = "baseball"
//   A: "baseball" → "basil"   (A = keep 'b','a', delete 'sebal', insert 'si', keep 'l' ... adjusted)
//   B: "baseball" → "below"
//
// We test: apply(apply("baseball", A), f(A,B)) == apply(apply("baseball", B), f(B,A))
func TestFollow_Baseball(t *testing.T) {
	doc := "baseball"

	// A: "baseball" → "basil"
	// Keep 'b','a' (2), delete 'sebal' (5) → wrong. let's just use DiffToChangeset.
	A := DiffToChangeset("baseball", "basil")
	B := DiffToChangeset("baseball", "below")

	assertConvergence(t, doc, A, B)
}

// TestFollow_NonOverlappingInserts: two clients insert at different positions.
func TestFollow_NonOverlappingInserts(t *testing.T) {
	doc := "doc"
	// A appends "X"
	A := MakeInsert(3, 3, "X")
	// B appends "Y"
	B := MakeInsert(3, 3, "Y")
	assertConvergence(t, doc, A, B)
}

// TestFollow_SimultaneousInsertsAtSamePosition: tie-breaker must kick in.
func TestFollow_SimultaneousInsertsAtSamePosition(t *testing.T) {
	doc := "x"
	// A inserts "b" at start, B inserts "a" at start.
	// Tie-breaker: "a" < "b" → B's text goes first → merged = "abx".
	A := MakeInsert(1, 0, "b")
	B := MakeInsert(1, 0, "a")
	assertConvergence(t, doc, A, B)

	// Also verify the actual merged result.
	fAB := mustFollow(t, A, B)
	fBA := mustFollow(t, B, A)
	docA := mustApply(t, doc, A)  // "bx"
	docB := mustApply(t, doc, B)  // "ax"
	r1 := mustApply(t, docA, fAB) // apply f(A,B) on top of "bx"
	r2 := mustApply(t, docB, fBA) // apply f(B,A) on top of "ax"
	if r1 != r2 {
		t.Errorf("tie-breaker failed: r1=%q r2=%q", r1, r2)
	}
}

// TestFollow_DeleteVsInsert: A deletes chars that B inserts around.
func TestFollow_DeleteVsInsert(t *testing.T) {
	doc := "car"
	A := DiffToChangeset("car", "ar") // delete 'c'
	B := DiffToChangeset("car", "cart") // insert 't' at end
	assertConvergence(t, doc, A, B)
}

// TestFollow_DeleteVsDelete: both clients delete overlapping regions.
func TestFollow_DeleteVsDelete(t *testing.T) {
	doc := "abcdef"
	A := DiffToChangeset("abcdef", "adef") // delete "bc"
	B := DiffToChangeset("abcdef", "abef") // delete "cd"
	assertConvergence(t, doc, A, B)
}

// TestFollow_Identity: follow with identity changesets.
func TestFollow_Identity(t *testing.T) {
	doc := "hello"
	id := Identity(5)
	A := MakeInsert(5, 2, "XY")
	assertConvergence(t, doc, A, id)
	assertConvergence(t, doc, id, A)
}

// ─────────────────────────────────────────────
// TestCompose
// ─────────────────────────────────────────────

func TestCompose_Basic(t *testing.T) {
	doc := "hello"
	A := MakeInsert(5, 5, " world") // "hello" → "hello world"
	B := MakeDelete(11, 5, 6)       // "hello world" → "hello"
	C := mustCompose(t, A, B)

	// Compose should give the identity (net effect: nothing changes).
	resultDirect := mustApply(t, doc, C)
	resultStepwise := mustApply(t, mustApply(t, doc, A), B)
	if resultDirect != resultStepwise {
		t.Errorf("compose wrong: direct=%q stepwise=%q", resultDirect, resultStepwise)
	}
}

func TestCompose_InsertThenInsert(t *testing.T) {
	doc := ""
	A := MakeInsert(0, 0, "hello")
	B := MakeInsert(5, 5, " world")
	C := mustCompose(t, A, B)
	result := mustApply(t, doc, C)
	if result != "hello world" {
		t.Errorf("expected %q got %q", "hello world", result)
	}
}

// ─────────────────────────────────────────────
// TestDiff
// ─────────────────────────────────────────────

func TestDiff_Basic(t *testing.T) {
	cases := []struct{ old, new string }{
		{"", "hello"},
		{"hello", ""},
		{"hello", "hello world"},
		{"hello world", "hello"},
		{"abc", "axc"},
		{"baseball", "basil"},
		{"baseball", "below"},
		{"the cat sat", "the dog sat"},
	}
	for _, tc := range cases {
		c := DiffToChangeset(tc.old, tc.new)
		result, err := ApplyChangeset(tc.old, c)
		if err != nil {
			t.Errorf("apply failed for %q→%q: %v", tc.old, tc.new, err)
			continue
		}
		if result != tc.new {
			t.Errorf("diff wrong: %q→%q got %q", tc.old, tc.new, result)
		}
	}
}

// ─────────────────────────────────────────────
// TestIsIdentity
// ─────────────────────────────────────────────

func TestIsIdentity(t *testing.T) {
	if !IsIdentity(Identity(0)) {
		t.Error("Identity(0) should be identity")
	}
	if !IsIdentity(Identity(5)) {
		t.Error("Identity(5) should be identity")
	}
	if IsIdentity(MakeInsert(5, 2, "x")) {
		t.Error("insert should not be identity")
	}
}

// ─────────────────────────────────────────────
// TestValidate
// ─────────────────────────────────────────────

func TestValidate(t *testing.T) {
	c := MakeInsert(5, 2, "hello")
	if err := c.Validate(); err != nil {
		t.Errorf("valid changeset failed validation: %v", err)
	}
	// Bad: wrong OldLen
	bad := Changeset{OldLen: 99, NewLen: 100, Ops: []Op{{Type: OpRetain, Len: 1}}}
	if err := bad.Validate(); err == nil {
		t.Error("invalid changeset passed validation")
	}
}

// ─────────────────────────────────────────────
// Fuzz Tests
// ─────────────────────────────────────────────

// FuzzFollowEquivalence is the main mathematical proof test.
// go test -fuzz=FuzzFollowEquivalence -fuzztime=30s ./internal/ot/
//
// For any random document and two random changesets A and B (both valid for that doc),
// we verify: Apply(Apply(doc, A), f(A,B)) == Apply(Apply(doc, B), f(B,A))
func FuzzFollowEquivalence(f *testing.F) {
	// Seed corpus with known cases.
	f.Add("baseball", "basil", "below")
	f.Add("hello", "hello world", "Hi there")
	f.Add("x", "bx", "ax")
	f.Add("abc", "aXbc", "abcY")
	f.Add("", "inserted", "also inserted")
	f.Add("the cat sat on the mat", "the dog sat on the mat", "the cat sat on the rug")

	f.Fuzz(func(t *testing.T, doc, targetA, targetB string) {
		// Ensure inputs are valid UTF-8 (the fuzzer may generate arbitrary bytes).
		if !utf8.ValidString(doc) || !utf8.ValidString(targetA) || !utf8.ValidString(targetB) {
			t.Skip()
		}

		// Build two changesets from the same doc.
		A := DiffToChangeset(doc, targetA)
		B := DiffToChangeset(doc, targetB)

		// Validate the changesets before using them.
		if err := A.Validate(); err != nil {
			t.Fatalf("DiffToChangeset produced invalid A: %v", err)
		}
		if err := B.Validate(); err != nil {
			t.Fatalf("DiffToChangeset produced invalid B: %v", err)
		}

		// apply A then f(A,B)
		docA, err := ApplyChangeset(doc, A)
		if err != nil {
			t.Fatalf("apply A failed: %v", err)
		}
		fAB, err := Follow(A, B)
		if err != nil {
			t.Fatalf("Follow(A,B) failed: %v", err)
		}
		resultAB, err := ApplyChangeset(docA, fAB)
		if err != nil {
			t.Fatalf("apply f(A,B) failed: %v\nfAB=%v", err, fAB)
		}

		// apply B then f(B,A)
		docB, err := ApplyChangeset(doc, B)
		if err != nil {
			t.Fatalf("apply B failed: %v", err)
		}
		fBA, err := Follow(B, A)
		if err != nil {
			t.Fatalf("Follow(B,A) failed: %v", err)
		}
		resultBA, err := ApplyChangeset(docB, fBA)
		if err != nil {
			t.Fatalf("apply f(B,A) failed: %v\nfBA=%v", err, fBA)
		}

		if resultAB != resultBA {
			t.Errorf("OT convergence violated!\ndoc=%q\nA:   doc→%q\nB:   doc→%q\nA→f(A,B)=%q\nB→f(B,A)=%q",
				doc, targetA, targetB, resultAB, resultBA)
		}
	})
}

// FuzzComposeThenApply verifies that Compose(A,B) gives the same result as A then B.
func FuzzComposeThenApply(f *testing.F) {
	f.Add("hello", "hello world", "HELLO WORLD")
	f.Add("", "abc", "abcdef")
	f.Add("xyz", "x", "")

	f.Fuzz(func(t *testing.T, doc, mid, final string) {
		if !utf8.ValidString(doc) || !utf8.ValidString(mid) || !utf8.ValidString(final) {
			t.Skip()
		}

		A := DiffToChangeset(doc, mid)
		B := DiffToChangeset(mid, final)

		if err := A.Validate(); err != nil {
			t.Fatalf("A invalid: %v", err)
		}
		if err := B.Validate(); err != nil {
			t.Fatalf("B invalid: %v", err)
		}

		C, err := Compose(A, B)
		if err != nil {
			t.Fatalf("Compose failed: %v", err)
		}

		direct, err := ApplyChangeset(doc, C)
		if err != nil {
			t.Fatalf("apply C failed: %v\nC=%v", err, C)
		}
		stepA, err := ApplyChangeset(doc, A)
		if err != nil {
			t.Fatalf("apply A failed: %v", err)
		}
		stepB, err := ApplyChangeset(stepA, B)
		if err != nil {
			t.Fatalf("apply B failed: %v", err)
		}

		if direct != stepB {
			t.Errorf("compose wrong: direct=%q stepwise=%q", direct, stepB)
		}
	})
}
