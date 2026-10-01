#!/usr/bin/env python3
"""
Put a signed manifest inside the binary it describes.

The manifest hashes the binary's own code pages, so it cannot be part of
the code: it goes into the reserved .note.attest section (see
pkg/attest/note_linux_amd64.S), which is patched in place after the link.
Nothing moves, so the hashes stay true, and this checks that before writing.

    scripts/sign-elf.py --key operator.pem -n gpu-metrics-exporter \\
        --version 1 bin/gpu-metrics-exporter

The device needs only the matching public key:

    openssl rsa -in operator.pem -pubout -RSAPublicKey_out -outform DER \\
        -out operator-pub.der
    qemu-system-x86_64 -device pci-attest,pubkey=operator-pub.der,...

--verify checks a signed binary the way the device will, against that same
public key, and writes nothing:

    scripts/sign-elf.py --verify operator-pub.der -n gpu-metrics-exporter \\
        --version 1 bin/gpu-metrics-exporter

Two more modes exist for the test stand, to produce binaries that must be
refused: --copy-note-from steals another binary's signed note, --tamper
corrupts the manifest under a good signature.
"""

import argparse
import base64
import binascii
import hashlib
import json
import os
import struct
import subprocess
import sys
import tempfile

NOTE_SECTION = ".note.attest"
NOTE_NAME = b"QEMU-ATTEST\0"
NOTE_TYPE = 1
DESC_MAGIC = 0x314D5441          # "ATM1"
DESC_HDR = 16

PAGE = 4096
PT_LOAD = 1
PT_NOTE = 4
PF_X = 1

# Limits the device applies to what it reads out of the guest.
MAX_NOTE_SEGMENT = 256 * 1024    # a larger PT_NOTE is skipped
MAX_MANIFEST = 256 * 1024


# ---------------------------------------------------------------------------
# The manifest: the page allow-list for the binary.
#
# The runtime check hashes the process' code pages exactly as the guest kernel
# maps them, i.e. the range [mm->start_code, mm->end_code) rounded out to whole
# pages.  This reproduces that mapping from the ELF file, so the two sides
# agree byte for byte.
# ---------------------------------------------------------------------------

def parse_phdrs(data):
    if data[:4] != b"\x7fELF":
        raise SystemExit("not an ELF file")
    if data[4] != 2:
        raise SystemExit("only 64-bit ELF is supported")
    end = "<" if data[5] == 1 else ">"

    (e_phoff,) = struct.unpack_from(end + "Q", data, 0x20)
    e_phentsize, e_phnum = struct.unpack_from(end + "HH", data, 0x36)

    phdrs = []
    for i in range(e_phnum):
        off = e_phoff + i * e_phentsize
        p_type, p_flags, p_offset, p_vaddr, _, p_filesz, p_memsz, _ = \
            struct.unpack_from(end + "IIQQQQQQ", data, off)
        if p_type == PT_LOAD:
            phdrs.append({
                "flags": p_flags,
                "offset": p_offset,
                "vaddr": p_vaddr,
                "filesz": p_filesz,
                "memsz": p_memsz,
            })
    if not phdrs:
        raise SystemExit("no PT_LOAD segments")
    return phdrs


def seg_for_page(phdrs, va):
    """Segment whose file mapping covers the whole page starting at va.

    mmap() is page granular, so within one PT_LOAD the page at va always
    holds the file bytes at offset + (va - vaddr), including the slack
    between p_filesz and the end of the page.  Prefer an executable segment
    when two of them share a page.
    """
    hits = []
    for p in phdrs:
        lo = p["vaddr"] & ~(PAGE - 1)
        hi = p["vaddr"] + p["memsz"]
        if lo <= va < hi:
            hits.append(p)
    if not hits:
        return None
    hits.sort(key=lambda p: 0 if p["flags"] & PF_X else 1)
    return hits[0]


def page_bytes(seg, data, va):
    off = seg["offset"] + (va - seg["vaddr"])
    if off < 0:
        return None
    chunk = data[off:off + PAGE]
    return chunk + b"\x00" * (PAGE - len(chunk))


def build_manifest(data, name, version=0):
    """The allow-list entry for the ELF image in @data."""
    phdrs = parse_phdrs(data)
    xsegs = [p for p in phdrs if p["flags"] & PF_X]
    if not xsegs:
        raise SystemExit("no executable PT_LOAD segment")

    # Mirrors fs/binfmt_elf.c, minus the load bias (which is position
    # dependent and therefore not part of the identity).
    start_code = min(p["vaddr"] for p in xsegs)
    end_code = max(p["vaddr"] + p["filesz"] for p in xsegs)

    base = start_code & ~(PAGE - 1)
    npages = (end_code - base + PAGE - 1) // PAGE

    pages = []
    for i in range(npages):
        va = base + i * PAGE
        seg = seg_for_page(phdrs, va)
        buf = page_bytes(seg, data, va) if seg else None
        # A page with no file backing cannot be pinned; the runtime skips it.
        pages.append(hashlib.sha256(buf).hexdigest() if buf else None)

    return {
        "name": name,
        "version": version,
        "code_len": end_code - start_code,
        "code_start_offset_in_page": start_code & (PAGE - 1),
        "npages": npages,
        "pages": pages,
    }


# ---------------------------------------------------------------------------
# The note the manifest is signed into.
# ---------------------------------------------------------------------------

def sections(data):
    """Yield (name, offset, size) for every section header."""
    if data[:4] != b"\x7fELF" or data[4] != 2:
        raise SystemExit("not a 64-bit ELF file")
    e_shoff, = struct.unpack_from("<Q", data, 0x28)
    e_shentsize, e_shnum, e_shstrndx = struct.unpack_from("<HHH", data, 0x3A)
    if not e_shoff or not e_shnum:
        raise SystemExit("no section headers: was the binary stripped?")

    def hdr(i):
        off = e_shoff + i * e_shentsize
        sh_name, = struct.unpack_from("<I", data, off)
        sh_offset, sh_size = struct.unpack_from("<QQ", data, off + 0x18)
        return sh_name, sh_offset, sh_size

    _, str_off, str_size = hdr(e_shstrndx)
    strtab = data[str_off:str_off + str_size]
    for i in range(e_shnum):
        sh_name, sh_offset, sh_size = hdr(i)
        end = strtab.find(b"\0", sh_name)
        yield strtab[sh_name:end].decode(), sh_offset, sh_size


def find_note(data):
    """File offset and capacity of the note's descriptor."""
    for name, off, size in sections(data):
        if name != NOTE_SECTION:
            continue
        namesz, descsz, ntype = struct.unpack_from("<III", data, off)
        if ntype != NOTE_TYPE or namesz != len(NOTE_NAME):
            raise SystemExit("%s is not the note we expect" % NOTE_SECTION)
        if data[off + 12:off + 12 + namesz] != NOTE_NAME:
            raise SystemExit("%s carries the wrong owner name" % NOTE_SECTION)
        desc = off + 12 + ((namesz + 3) & ~3)
        if desc + descsz > off + size:
            raise SystemExit("note descriptor runs past its section")
        return desc, descsz
    raise SystemExit("no %s section: build with cgo so pkg/attest's "
                     "note_linux_amd64.S is linked in" % NOTE_SECTION)


def openssl_sign(key, payload):
    p = subprocess.run(["openssl", "dgst", "-sha256", "-sign", key],
                       input=payload, capture_output=True)
    if p.returncode:
        sys.stderr.write(p.stderr.decode(errors="replace"))
        raise SystemExit("openssl could not sign the manifest")
    return p.stdout


def patch(data, desc_off, desc_size, payload):
    if len(payload) > desc_size:
        raise SystemExit("manifest and signature need %d bytes, the note "
                         "reserves %d: raise .space in "
                         "pkg/attest/note_linux_amd64.S"
                         % (len(payload), desc_size))
    out = bytearray(data)
    out[desc_off:desc_off + desc_size] = payload.ljust(desc_size, b"\0")
    return bytes(out)


# ---------------------------------------------------------------------------
# --verify: what the device will make of a signed binary, short of running it.
# ---------------------------------------------------------------------------

def device_note(data):
    """The note's descriptor, found the way the device finds it.

    Not through the section headers, which never reach memory, but through
    the PT_NOTE program headers: each segment read where a PT_LOAD maps it
    from these very file bytes, its notes walked 4-byte aligned.
    """
    if data[:4] != b"\x7fELF" or data[4] != 2 or data[5] != 1:
        raise SystemExit("not a 64-bit little-endian ELF file")
    e_phoff, = struct.unpack_from("<Q", data, 0x20)
    e_phentsize, e_phnum = struct.unpack_from("<HH", data, 0x36)
    # (p_type, p_flags, p_offset, p_vaddr, p_paddr, p_filesz, p_memsz, p_align)
    phdrs = [struct.unpack_from("<IIQQQQQQ", data, e_phoff + i * e_phentsize)
             for i in range(e_phnum)]
    loads = [p for p in phdrs if p[0] == PT_LOAD]

    for p_type, _, p_offset, p_vaddr, _, p_filesz, _, _ in phdrs:
        if p_type != PT_NOTE or not p_filesz or p_filesz > MAX_NOTE_SEGMENT:
            continue
        if not any(ld[3] <= p_vaddr and p_vaddr + p_filesz <= ld[3] + ld[5] and
                   p_offset - ld[2] == p_vaddr - ld[3] for ld in loads):
            continue        # not mapped from the file: the device can't see it
        seg = data[p_offset:p_offset + p_filesz]
        off = 0
        while off + 12 <= len(seg):
            namesz, descsz, ntype = struct.unpack_from("<III", seg, off)
            desc_off = off + 12 + ((namesz + 3) & ~3)
            nxt = desc_off + ((descsz + 3) & ~3)
            if nxt > len(seg) or nxt <= off:
                break
            if ntype == NOTE_TYPE and namesz == len(NOTE_NAME) and \
               seg[off + 12:off + 12 + namesz] == NOTE_NAME:
                return seg[desc_off:desc_off + descsz]
            off = nxt
    raise SystemExit("the binary carries no attestation note the device can "
                     "find: none in a PT_NOTE segment that a PT_LOAD maps")


def load_pubkey(path):
    """The operator's public key as a PEM SubjectPublicKeyInfo, for openssl.

    Takes what QEMU takes (DER RSAPublicKey: operator-pub.der), PEM, or the
    base64 of the DER -- the form a CI secret can hold.
    """
    with open(path, "rb") as f:
        raw = f.read()
    if raw.lstrip().startswith(b"-----BEGIN"):
        candidates = [("PEM", raw)]
    else:
        candidates = [("DER", raw)]
        try:
            candidates.append(("DER", base64.b64decode(raw)))
        except (binascii.Error, ValueError):
            pass
    for form, blob in candidates:
        for cmd in (["openssl", "rsa", "-RSAPublicKey_in", "-inform", form,
                     "-pubout"],
                    ["openssl", "pkey", "-pubin", "-inform", form]):
            p = subprocess.run(cmd, input=blob, capture_output=True)
            if p.returncode == 0 and b"-----BEGIN PUBLIC KEY-----" in p.stdout:
                return p.stdout
    raise SystemExit("%s: not an RSA public key (DER, PEM, or base64 of "
                     "the DER)" % path)


def openssl_verify(pubkey_pem, payload, sig):
    """Whether sig is a PKCS#1 v1.5 RSA signature over the SHA-256 of
    payload: the check the device makes, and what openssl_sign produces."""
    with tempfile.TemporaryDirectory() as tmp:
        key = os.path.join(tmp, "pub.pem")
        sigfile = os.path.join(tmp, "sig")
        with open(key, "wb") as f:
            f.write(pubkey_pem)
        with open(sigfile, "wb") as f:
            f.write(sig)
        p = subprocess.run(["openssl", "dgst", "-sha256", "-verify", key,
                            "-signature", sigfile],
                           input=payload, capture_output=True)
    return p.returncode == 0


def verify(data, pubkey_pem, name=None, version=None):
    """Check a signed binary in the order the device does: the note where
    it looks, the signature over the raw manifest before a single field of
    it is read, then the manifest against this binary's own code -- the
    layout and every page.  name and version, when given, must be what it
    was signed as.  Raises SystemExit with the first thing that fails."""
    desc = device_note(data)
    if len(desc) < DESC_HDR or struct.unpack_from("<I", desc)[0] != DESC_MAGIC:
        raise SystemExit("the attestation note is empty or malformed: "
                         "was the binary signed?")
    _, mlen, slen, _ = struct.unpack_from("<IIII", desc)
    if not mlen or not slen or mlen > MAX_MANIFEST or \
       DESC_HDR + mlen + slen > len(desc):
        raise SystemExit("the attestation note has bad lengths")
    manifest = desc[DESC_HDR:DESC_HDR + mlen]
    sig = desc[DESC_HDR + mlen:DESC_HDR + mlen + slen]
    if not openssl_verify(pubkey_pem, manifest, sig):
        raise SystemExit("the manifest signature does not check out against "
                         "this public key")

    try:
        signed, = json.loads(manifest)["binaries"]
        pages = signed["pages"]
    except (ValueError, KeyError, TypeError):
        raise SystemExit("the signed manifest is not one binary's, as this "
                         "script writes it")

    # What the device will measure: the binary's [start_code, end_code) and
    # its pages, exactly as signing this binary would have described them.
    want = build_manifest(data, "", 0)
    for key in ("code_len", "code_start_offset_in_page", "npages"):
        if signed.get(key) != want[key]:
            raise SystemExit("the manifest's %s is %r, this binary's is %r: "
                             "it describes another build"
                             % (key, signed.get(key), want[key]))
    if len(pages) != want["npages"]:
        raise SystemExit("the manifest lists %d pages for %d"
                         % (len(pages), want["npages"]))
    for i, (got, exp) in enumerate(zip(pages, want["pages"])):
        if got != exp:
            raise SystemExit("code page %d does not match the manifest: the "
                             "binary changed after it was signed" % i)

    if name is not None and signed.get("name") != name:
        raise SystemExit("signed as %r, expected %r" % (signed.get("name"), name))
    if version is not None and signed.get("version") != version:
        raise SystemExit("signed as version %r, expected %d"
                         % (signed.get("version"), version))
    return ("signature OK, '%s' version %s, %d code pages match"
            % (signed.get("name"), signed.get("version"), want["npages"]))


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("binary")
    ap.add_argument("--key", help="RSA private key in PEM, to sign with")
    ap.add_argument("-n", "--name", help="label reported in the verdict")
    ap.add_argument("--version", type=int, default=None,
                    help="monotonic release number, signed into the manifest "
                         "(default 0); the device refuses anything below its "
                         "min-version.  With --verify, the version the "
                         "manifest must carry")
    ap.add_argument("--verify", metavar="PUBKEY",
                    help="check a signed binary as the device will, against "
                         "the operator's public key (operator-pub.der as QEMU "
                         "takes it, PEM, or base64 of the DER); writes nothing")
    ap.add_argument("--copy-note-from", metavar="ELF",
                    help="copy that binary's note instead of signing")
    ap.add_argument("--tamper", action="store_true",
                    help="flip a byte of the manifest, keeping the signature")
    args = ap.parse_args()
    if args.version is not None and args.version < 0:
        ap.error("--version must be >= 0")
    if args.verify and (args.key or args.copy_note_from or args.tamper):
        ap.error("--verify only checks; it takes no --key, --copy-note-from "
                 "or --tamper")

    with open(args.binary, "rb") as f:
        data = f.read()

    if args.verify:
        what = verify(data, load_pubkey(args.verify), args.name, args.version)
        print("%s: %s" % (args.binary, what), file=sys.stderr)
        return

    desc_off, desc_size = find_note(data)

    if args.copy_note_from:
        with open(args.copy_note_from, "rb") as f:
            other = f.read()
        src_off, src_size = find_note(other)
        if src_size != desc_size:
            raise SystemExit("the two notes are of different sizes")
        payload = other[src_off:src_off + src_size]
        out = patch(data, desc_off, desc_size, payload)
        what = "note copied from %s" % args.copy_note_from
    elif args.tamper:
        mlen, = struct.unpack_from("<I", data, desc_off + 4)
        if not mlen:
            raise SystemExit("nothing to tamper with: sign it first")
        out = bytearray(data)
        out[desc_off + DESC_HDR] ^= 0x20
        out = bytes(out)
        what = "manifest corrupted under a good signature"
    else:
        if not args.key:
            ap.error("--key is required to sign")
        name = args.name or os.path.basename(args.binary)
        manifest = json.dumps(
            {"binaries": [build_manifest(data, name, args.version or 0)]},
            indent=2).encode()
        sig = openssl_sign(args.key, manifest)
        payload = struct.pack("<IIII", DESC_MAGIC, len(manifest), len(sig), 0)
        payload += manifest + sig
        out = patch(data, desc_off, desc_size, payload)

        # The whole scheme rests on this: writing the note must not disturb
        # a single byte the manifest hashed.
        before = build_manifest(data, name)
        after = build_manifest(out, name)
        if before["pages"] != after["pages"] or \
           before["code_len"] != after["code_len"]:
            raise SystemExit("the note shares a page with the code it "
                             "describes; move .note.attest out of the "
                             "executable segment")
        what = ("%d code pages, %d byte manifest, %d byte signature"
                % (after["npages"], len(manifest), len(sig)))

    with open(args.binary, "wb") as f:
        f.write(out)
    print("%s: %s" % (args.binary, what), file=sys.stderr)


if __name__ == "__main__":
    main()
