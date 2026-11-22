package peek

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
)

const (
	// MaxCentralDirBytes caps the ZIP central directory size we will hash/read upfront.
	// 16 MB covers ~200,000 ZIP entries while protecting against pathological archives.
	MaxCentralDirBytes int64 = 16 * 1024 * 1024

	zipCDHeaderSignature  uint32 = 0x02014b50
	zipEOCDSignature      uint32 = 0x06054b50
	zip64EOCDLocSignature uint32 = 0x07064b50
	zip64EOCDSignature    uint32 = 0x06064b50
	zipEOCDMinLen         int64  = 22
	zipEOCDMaxCommentLen  int64  = 65535
	zip64EOCDLocatorLen   int64  = 20
	zip64EOCDMinRecordLen int64  = 56
	zipUint16Max          uint16 = 0xFFFF
	zipUint32Max          uint64 = 0xFFFFFFFF
)

// ErrTooLarge indicates the bounded metadata section needed to fingerprint/peek a file
// (such as a ZIP central directory) exceeds upfront safety limits.
var ErrTooLarge = errors.New("file metadata section exceeds upfront limit")

// Fingerprint computes a fast BLAKE3 digest over only the bounded bytes that Peek consumes:
//   - Plain text / Markdown: first 256 KB (when size <= 256 KB) or first 64 KB + truncated flag
//   - CSV / TSV: first 64 KB + truncated flag
//   - ZIP / XLSX / PPTX: raw ZIP central directory bytes (zero decompression; includes every
//     member's name, size, timestamp, and CRC32)
//
// Peeker separation is enforced structurally by the store's composite content_blobs key
// (blake3, peeker_id, peeker_version), so identical content under different extensions
// (e.g. a.csv vs b.txt, report.xlsx vs report.zip) stores independent cards while still
// sharing a content fingerprint.
//
// Returns ("", nil) if the extension is not peekable or if a ZIP/OOXML container lacks a valid
// central directory. Returns ErrTooLarge if a ZIP central directory exceeds MaxCentralDirBytes.
func Fingerprint(ctx context.Context, path string, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	ext := strings.ToLower(filepath.Ext(path))
	peekerID := PeekerIDForExt(ext)
	if peekerID == "" {
		return "", nil
	}

	switch peekerID {
	case "zip", "xlsx", "pptx":
		return fingerprintZipCentralDir(ctx, path, size)
	case "csv", "tsv":
		return fingerprintHead(ctx, path, MaxHeaderTextBytes)
	default:
		limit := int64(MaxFullTextBytes)
		if size > MaxFullTextBytes {
			limit = MaxHeaderTextBytes
		}
		return fingerprintHead(ctx, path, limit)
	}
}

func fingerprintHead(ctx context.Context, path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Read up to limit+1 bytes so we know whether content extends past the peek window.
	buf := make([]byte, limit+1)
	total := 0
	for total < len(buf) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf[total:])
		total += n
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}

	var truncated byte
	if int64(total) > limit {
		truncated = 1
		total = int(limit)
	}

	h := blake3.New()
	_, _ = h.Write([]byte("head:v1\x00"))
	_, _ = h.Write([]byte{truncated})
	_, _ = h.Write(buf[:total])
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fingerprintZipCentralDir(ctx context.Context, path string, size int64) (string, error) {
	if size < zipEOCDMinLen {
		return "", nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	cdOffset, cdSize, ok, err := locateZipCentralDir(f, size)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if cdSize > uint64(MaxCentralDirBytes) {
		return "", ErrTooLarge
	}

	h := blake3.New()
	_, _ = h.Write([]byte("zipcd:v1\x00"))

	sr := io.NewSectionReader(f, int64(cdOffset), int64(cdSize))
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := sr.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// locateZipCentralDir reads only the tail of a ZIP file (and ZIP64 EOCD if present)
// to find the actual central directory offset and byte length without decompressing any member.
//
// It matches Go's archive/zip behavior for:
//   - self-extracting / prepended-prefix ZIPs (deriving actual start from EOCD offset - cdSize)
//   - trailing bytes after the EOCD comment
//   - ZIP64 sentinels (records == 0xFFFF or cdSize/cdOffset == 0xFFFFFFFF)
//
// For non-empty archives it also verifies the PK\x01\x02 central directory header signature
// at the resolved offset so fake EOCD signatures inside comments or prepended stubs are rejected.
func locateZipCentralDir(r io.ReaderAt, size int64) (cdOffset uint64, cdSize uint64, ok bool, err error) {
	tailLen := min(size, zipEOCDMaxCommentLen+zipEOCDMinLen)
	tailStart := size - tailLen

	tail := make([]byte, tailLen)
	if _, err := r.ReadAt(tail, tailStart); err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, false, err
	}

	// Scan backwards for candidate EOCD records; validate each against the actual central directory.
	for i := len(tail) - int(zipEOCDMinLen); i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) != zipEOCDSignature {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(tail[i+20 : i+22]))
		if i+int(zipEOCDMinLen)+commentLen > len(tail) {
			continue
		}

		records := uint64(binary.LittleEndian.Uint16(tail[i+10 : i+12]))
		cdSize = uint64(binary.LittleEndian.Uint32(tail[i+12 : i+16]))
		headerCDOff := uint64(binary.LittleEndian.Uint32(tail[i+16 : i+20]))
		eocdFileOff := tailStart + int64(i)
		dirEndOff := eocdFileOff

		if records == uint64(zipUint16Max) || cdSize == zipUint32Max || headerCDOff == zipUint32Max {
			if z64Records, z64Size, z64Off, z64RecOff, found := readZip64EOCD(r, size, eocdFileOff); found {
				records = z64Records
				cdSize = z64Size
				headerCDOff = z64Off
				dirEndOff = z64RecOff
			} else if cdSize == zipUint32Max || headerCDOff == zipUint32Max {
				continue
			}
		}

		// Compute actual central directory start relative to dirEndOff (handles prepended stubs
		// just like Go's archive/zip: baseOffset = dirEndOff - cdSize - headerCDOff, so
		// actualCDStart = baseOffset + headerCDOff = dirEndOff - cdSize).
		if cdSize > uint64(dirEndOff) {
			continue
		}
		actualStart := uint64(dirEndOff) - cdSize
		if actualStart > uint64(size) || cdSize > uint64(size)-actualStart {
			continue
		}

		if cdSize == 0 {
			if records != 0 {
				continue
			}
			return actualStart, 0, true, nil
		}

		// Verify PK\x01\x02 at the resolved central directory start (and fall back to headerCDOff
		// if an archive places data between the CD and EOCD).
		if hasCDHeaderAt(r, int64(actualStart)) {
			return actualStart, cdSize, true, nil
		}
		if headerCDOff != actualStart && headerCDOff <= uint64(size) && cdSize <= uint64(size)-headerCDOff && hasCDHeaderAt(r, int64(headerCDOff)) {
			return headerCDOff, cdSize, true, nil
		}
	}

	return 0, 0, false, nil
}

func readZip64EOCD(r io.ReaderAt, size int64, eocdFileOff int64) (records, cdSize, cdOffset uint64, recFileOff int64, ok bool) {
	if eocdFileOff < zip64EOCDLocatorLen {
		return 0, 0, 0, 0, false
	}
	var loc [zip64EOCDLocatorLen]byte
	if _, err := r.ReadAt(loc[:], eocdFileOff-zip64EOCDLocatorLen); err != nil {
		return 0, 0, 0, 0, false
	}
	if binary.LittleEndian.Uint32(loc[0:4]) != zip64EOCDLocSignature {
		return 0, 0, 0, 0, false
	}
	z64Off := binary.LittleEndian.Uint64(loc[8:16])
	if z64Off > uint64(size) || uint64(size)-z64Off < uint64(zip64EOCDMinRecordLen) {
		return 0, 0, 0, 0, false
	}
	var rec [zip64EOCDMinRecordLen]byte
	if _, err := r.ReadAt(rec[:], int64(z64Off)); err != nil {
		return 0, 0, 0, 0, false
	}
	if binary.LittleEndian.Uint32(rec[0:4]) != zip64EOCDSignature {
		return 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint64(rec[32:40]),
		binary.LittleEndian.Uint64(rec[40:48]),
		binary.LittleEndian.Uint64(rec[48:56]),
		int64(z64Off),
		true
}

func hasCDHeaderAt(r io.ReaderAt, offset int64) bool {
	var sig [4]byte
	if _, err := r.ReadAt(sig[:], offset); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(sig[:]) == zipCDHeaderSignature
}
