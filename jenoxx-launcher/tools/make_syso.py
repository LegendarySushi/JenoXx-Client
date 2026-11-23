#!/usr/bin/env python3
"""Erzeugt rsrc_windows_amd64.syso (Icon-Ressource) aus assets/icon.ico - ohne Zusatzpakete."""
import struct, sys

ico_path = sys.argv[1] if len(sys.argv) > 1 else "assets/icon.ico"
out_path = sys.argv[2] if len(sys.argv) > 2 else "rsrc_windows_amd64.syso"

ico = open(ico_path, "rb").read()
_, itype, count = struct.unpack_from("<HHH", ico, 0)
assert itype == 1 and count > 0
images = []  # (w,h,colors,reserved,planes,bpp,data)
for i in range(count):
    w, h, cc, rs, pl, bpp, size, off = struct.unpack_from("<BBBBHHII", ico, 6 + 16 * i)
    images.append((w, h, cc, rs, pl, bpp, ico[off:off + size]))

LANG = 0x0409

def align4(b):
    return b + b"\0" * (-len(b) % 4)

# Rohdaten: Icons (IDs 1..n) und Gruppen-Icon
grp = struct.pack("<HHH", 0, 1, count)
for i, (w, h, cc, rs, pl, bpp, data) in enumerate(images, 1):
    grp += struct.pack("<BBBBHHIH", w, h, cc, rs, pl, bpp, len(data), i)
datas = [(i, d[6]) for i, d in enumerate(images, 1)] + [("grp", grp)]
raw_blobs = [d[6] for d in images] + [grp]

# Layout berechnen
DIR, ENT, DE = 16, 8, 16
n = count
root_size = DIR + 2 * ENT
icon_dir_size = DIR + n * ENT
grp_dir_size = DIR + 1 * ENT
lang_dir_size = DIR + ENT  # je Name ein Sprach-Verzeichnis

off_root = 0
off_icon_dir = off_root + root_size
off_grp_dir = off_icon_dir + icon_dir_size
off_lang = off_grp_dir + grp_dir_size            # n + 1 Sprach-Verzeichnisse
off_de = off_lang + (n + 1) * lang_dir_size      # n + 1 Datenelemente
off_data = off_de + (n + 1) * DE

# Datenoffsets
blob_offsets, cur = [], off_data
for b in raw_blobs:
    blob_offsets.append(cur)
    cur += len(align4(b))
total = cur

def directory(n_named, n_id):
    return struct.pack("<IIHHHH", 0, 0, 0, 0, n_named, n_id)

res = bytearray()
# Wurzel: RT_ICON (3), RT_GROUP_ICON (14)
res += directory(0, 2)
res += struct.pack("<II", 3, 0x80000000 | off_icon_dir)
res += struct.pack("<II", 14, 0x80000000 | off_grp_dir)
# RT_ICON: Namen 1..n -> Sprachverzeichnis
res += directory(0, n)
for i in range(n):
    res += struct.pack("<II", i + 1, 0x80000000 | (off_lang + i * lang_dir_size))
# RT_GROUP_ICON: Name 1 -> Sprachverzeichnis n
res += directory(0, 1)
res += struct.pack("<II", 1, 0x80000000 | (off_lang + n * lang_dir_size))
# Sprachverzeichnisse
for i in range(n + 1):
    res += directory(0, 1)
    res += struct.pack("<II", LANG, off_de + i * DE)
# Datenelemente (OffsetToData wird per Relocation zur RVA)
reloc_positions = []
for i in range(n + 1):
    reloc_positions.append(len(res))
    res += struct.pack("<IIII", blob_offsets[i], len(raw_blobs[i]), 0, 0)
assert len(res) == off_data, (len(res), off_data)
for b in raw_blobs:
    res += align4(b)
assert len(res) == total

# COFF-Objektdatei
FILE_HDR, SEC_HDR = 20, 40
ptr_raw = FILE_HDR + SEC_HDR
ptr_reloc = ptr_raw + len(res)
nrel = len(reloc_positions)
ptr_sym = ptr_reloc + nrel * 10

obj = struct.pack("<HHIIIHH", 0x8664, 1, 0, ptr_sym, 1, 0, 0)
obj += struct.pack("<8sIIIIIIHHI", b".rsrc", 0, 0, len(res), ptr_raw, ptr_reloc, 0, nrel, 0, 0xC0000040)
obj += bytes(res)
for pos in reloc_positions:
    obj += struct.pack("<IIH", pos, 0, 3)  # IMAGE_REL_AMD64_ADDR32NB, Symbol 0
obj += struct.pack("<8sIhHBB", b".rsrc", 0, 1, 0, 3, 0)  # Symbol .rsrc (statisch)
obj += struct.pack("<I", 4)  # leere String-Tabelle
open(out_path, "wb").write(obj)
print("geschrieben:", out_path, len(obj), "Bytes,", count, "Icon-Groessen")
