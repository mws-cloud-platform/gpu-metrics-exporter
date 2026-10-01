/*
 * Reserved room for the attestation manifest, inside the binary it
 * describes -- the Go/cgo counterpart of the QEMU tree's
 * contrib/pci-attest/guest/attest-note.S.
 *
 * An allocated ELF note the external linker places in a PT_LOAD (and points a
 * PT_NOTE at), so it is mapped into the process image and the device finds it
 * through AT_PHDR.  The space is reserved here at build time and filled in
 * afterwards by scripts/sign-elf.py, which patches the file in place without
 * moving any code.  cgo compiles this .S with the C toolchain (gcc as), so it
 * lands in the final binary via the external linker.
 *
 * A Go binary has far more code than the C sample agent, so the reserved
 * space is 128 KiB.  If sign-elf.py reports the manifest does not fit, raise
 * it (ceiling ~255 KiB, counting neighbouring notes) or trim dependencies.
 */

	.section .note.attest, "a", @note
	.balign 4
	.globl attest_note
attest_note:
	.long	12				/* n_namesz: "QEMU-ATTEST\0" */
	.long	attest_desc_end - attest_desc	/* n_descsz */
	.long	1				/* n_type */
	.asciz	"QEMU-ATTEST"
	.balign 4
attest_desc:
	.long	0		/* magic "ATM1", written by sign-elf.py */
	.long	0		/* manifest length */
	.long	0		/* signature length */
	.long	0		/* reserved */
	.space	131072, 0
attest_desc_end:
	.size	attest_note, . - attest_note

	.section .note.GNU-stack, "", @progbits
