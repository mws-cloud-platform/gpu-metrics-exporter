//go:build cgo && linux && amd64

// This file pulls the reserved .note.attest space (note_linux_amd64.S) into
// the build and keeps the linker from dropping it.  cgo compiles the C files
// and hand-written assembly in the package when a Go file imports "C", so the
// exporter must be built with CGO_ENABLED=1.

package attest

/*
extern const void *attest_note_ptr(void);
*/
import "C"

func init() {
	// Touch the note symbol so nothing garbage-collects the section.
	// attest_note_ptr (note_ref_linux_amd64.c) returns attest_note
	// (note_linux_amd64.S).
	_ = C.attest_note_ptr()
}
