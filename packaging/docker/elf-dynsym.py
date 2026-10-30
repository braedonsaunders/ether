#!/usr/bin/env python3
"""Print whether an ELF dynamic symbol is defined, undefined, or absent.

Usage: elf-dynsym.py SYMBOL FILE [FILE...]
Each line is: STATUS PATH
STATUS is DEFINED, UNDEFINED, ABSENT, or NOTELF.
"""

import struct
import sys

SHT_DYNSYM = 11
SHN_UNDEF = 0


def _u(fmt, data, off):
    return struct.unpack_from(fmt, data, off)


def dynsym_status(path, symbol):
    with open(path, "rb") as fh:
        data = fh.read()
    if len(data) < 64 or data[:4] != b"\x7fELF":
        return "NOTELF"
    elfclass = data[4]
    if data[5] != 1:
        raise SystemExit(f"unsupported ELF endian: {path}")
    if elfclass == 2:
        e_shoff = _u("<Q", data, 40)[0]
        e_shentsize, e_shnum = _u("<HH", data, 58)
        sym_fmt = "<IBBHQQ"
        sym_size = 24
        name_off = 0
        shndx_off = 6
    elif elfclass == 1:
        e_shoff = _u("<I", data, 32)[0]
        e_shentsize, e_shnum = _u("<HH", data, 46)
        sym_fmt = "<IIIBBH"
        sym_size = 16
        name_off = 0
        shndx_off = 14
    else:
        raise SystemExit(f"unsupported ELF class: {path}")
    if e_shoff == 0 or e_shnum == 0:
        return "ABSENT"
    defined = False
    undefined = False
    for i in range(e_shnum):
        sh = e_shoff + i * e_shentsize
        sh_type, sh_offset, sh_size, sh_link, sh_entsize = _section(data, sh, elfclass)
        if sh_type != SHT_DYNSYM or sh_entsize == 0:
            continue
        strtab = _strtab(data, e_shoff, e_shentsize, sh_link, elfclass)
        count = sh_size // sh_entsize
        for n in range(count):
            sym = sh_offset + n * sym_size
            if sym + sym_size > len(data):
                break
            st_name = _u("<I", data, sym + name_off)[0]
            st_shndx = _u("<H", data, sym + shndx_off)[0]
            end = strtab.find(b"\x00", st_name)
            if end < 0:
                continue
            name = strtab[st_name:end]
            if name != symbol.encode():
                continue
            if st_shndx == SHN_UNDEF:
                undefined = True
            else:
                defined = True
    if defined:
        return "DEFINED"
    if undefined:
        return "UNDEFINED"
    return "ABSENT"


def _section(data, sh, elfclass):
    if elfclass == 2:
        sh_type = _u("<I", data, sh + 4)[0]
        sh_offset = _u("<Q", data, sh + 24)[0]
        sh_size = _u("<Q", data, sh + 32)[0]
        sh_link = _u("<I", data, sh + 40)[0]
        sh_entsize = _u("<Q", data, sh + 56)[0]
    else:
        sh_type = _u("<I", data, sh + 4)[0]
        sh_offset = _u("<I", data, sh + 16)[0]
        sh_size = _u("<I", data, sh + 20)[0]
        sh_link = _u("<I", data, sh + 24)[0]
        sh_entsize = _u("<I", data, sh + 36)[0]
    return sh_type, sh_offset, sh_size, sh_link, sh_entsize


def _strtab(data, e_shoff, e_shentsize, index, elfclass):
    sh = e_shoff + index * e_shentsize
    _, sh_offset, sh_size, _, _ = _section(data, sh, elfclass)
    return data[sh_offset : sh_offset + sh_size]


def main(argv):
    if len(argv) >= 4 and argv[1] == "--contains":
        symbol = argv[2].encode()
        for path in argv[3:]:
            with open(path, "rb") as fh:
                present = symbol in fh.read()
            print(("CONTAINS" if present else "MISSING") + "\t" + path)
        return 0
    if len(argv) < 3:
        print("usage: elf-dynsym.py SYMBOL FILE [FILE...]", file=sys.stderr)
        print("       elf-dynsym.py --contains SYMBOL FILE [FILE...]", file=sys.stderr)
        return 2
    symbol = argv[1]
    for path in argv[2:]:
        print(f"{dynsym_status(path, symbol)}\t{path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
