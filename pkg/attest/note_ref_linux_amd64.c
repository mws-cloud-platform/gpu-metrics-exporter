/*
 * A one-line reference to the attest_note symbol defined in
 * note_linux_amd64.S, so nothing garbage-collects the note section.  Kept in
 * a separate, asm-free C file so the compiler's debug-line machinery has no
 * custom section to trip over.
 */
extern char attest_note[];

const void *attest_note_ptr(void)
{
    return attest_note;
}
