# Independent reference implementation of the TorrentZip format using the
# system zlib. Reads a JSON list of [name, base64 data] pairs on stdin and
# writes the archive to stdout.
import base64, json, struct, sys, zlib


def key(name):
    # Compare UTF-16 code units with ASCII letters folded to lower case.
    units = struct.unpack('<%dH' % (len(name.encode('utf-16-le')) // 2), name.encode('utf-16-le'))
    folded = tuple(u + 32 if 65 <= u <= 90 else u for u in units)
    return (folded, units)


# Code page 437 as RomVault encodes it: printable ASCII as is, the glyphs
# shown for bytes 0x00-0x1f, and the upper half of the code page. ASCII
# control characters are not representable.
GLYPHS = ('\u0000\u263a\u263b\u2665\u2666\u2663\u2660\u2022\u25d8\u25cb\u25d9\u2642\u2640\u266a\u266b\u263c'
          '\u25ba\u25c4\u2195\u203c\u00b6\u00a7\u25ac\u21a8\u2191\u2193\u2192\u2190\u221f\u2194\u25b2\u25bc')
CP437 = {c: i for i, c in enumerate(GLYPHS)}
CP437.update({chr(i): i for i in range(32, 128)})
CP437.update({bytes([i]).decode('cp437'): i for i in range(128, 256)})


def encode(name):
    if all(c in CP437 for c in name):
        return bytes(CP437[c] for c in name), 0
    return name.encode('utf-8'), 0x0800


files = sorted(json.load(sys.stdin), key=lambda f: key(f[0]))
files = [f for i, f in enumerate(files)
         if not (f[0].endswith('/') and i + 1 < len(files) and files[i + 1][0].lower().startswith(f[0].lower()))]

out = bytearray()
central = bytearray()
for name, data in files:
    data = base64.b64decode(data)
    c = zlib.compressobj(9, zlib.DEFLATED, -15, 8, zlib.Z_DEFAULT_STRATEGY)
    comp = c.compress(data) + c.flush()
    raw, flag = encode(name)
    crc = zlib.crc32(data)
    offset = len(out)
    out += struct.pack('<IHHHHHIIIHH', 0x04034b50, 20, 2 | flag, 8, 0xbc00, 0x2198, crc, len(comp), len(data), len(raw), 0)
    out += raw + comp
    central += struct.pack('<IHHHHHHIIIHHHHHII', 0x02014b50, 0, 20, 2 | flag, 8, 0xbc00, 0x2198, crc, len(comp), len(data), len(raw), 0, 0, 0, 0, 0, offset)
    central += raw

comment = b'TORRENTZIPPED-%08X' % zlib.crc32(central)
start = len(out)
out += central
out += struct.pack('<IHHHHIIH', 0x06054b50, 0, 0, len(files), len(files), len(central), start, len(comment)) + comment
sys.stdout.buffer.write(out)
