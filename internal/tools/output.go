package tools

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	cmdOutputHeadBytes = 4 * 1024
	cmdOutputTailBytes = 12 * 1024
)

var (
	ansiCSIRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	ansiOSCRegex = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
)

// headTailBuffer retains the first headCap bytes and last tailCap bytes of a stream
// while tracking the total byte count and binary-detection heuristics.
type headTailBuffer struct {
	headCap int
	tailCap int

	head       []byte
	tail       []byte // ring buffer of size tailCap once overflow begins
	tailPos    int
	tailFilled bool
	total      int64
	hasNUL     bool
}

func newHeadTailBuffer(headCap, tailCap int) *headTailBuffer {
	return &headTailBuffer{
		headCap: headCap,
		tailCap: tailCap,
		head:    make([]byte, 0, min(headCap, 4096)),
	}
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	b.total += int64(origLen)
	if !b.hasNUL && bytes.IndexByte(p, 0) >= 0 {
		b.hasNUL = true
	}

	// Fill head first.
	if len(b.head) < b.headCap {
		need := b.headCap - len(b.head)
		if len(p) <= need {
			b.head = append(b.head, p...)
			return origLen, nil
		}
		b.head = append(b.head, p[:need]...)
		p = p[need:]
	}

	// Write remainder into circular tail buffer.
	if b.tailCap <= 0 || len(p) == 0 {
		return origLen, nil
	}
	if b.tail == nil {
		b.tail = make([]byte, b.tailCap)
	}
	if len(p) >= b.tailCap {
		copy(b.tail, p[len(p)-b.tailCap:])
		b.tailPos = 0
		b.tailFilled = true
		return origLen, nil
	}
	first := b.tailCap - b.tailPos
	if len(p) <= first {
		copy(b.tail[b.tailPos:], p)
		b.tailPos += len(p)
		if b.tailPos == b.tailCap {
			b.tailPos = 0
			b.tailFilled = true
		}
	} else {
		copy(b.tail[b.tailPos:], p[:first])
		rem := len(p) - first
		copy(b.tail[0:], p[first:])
		b.tailPos = rem
		b.tailFilled = true
	}
	return origLen, nil
}

func (b *headTailBuffer) Format() (text string, totalBytes int64, truncated bool) {
	totalBytes = b.total
	if b.total == 0 {
		return "", 0, false
	}

	tailBytes := b.orderedTail()
	combinedSample := make([]byte, 0, len(b.head)+len(tailBytes))
	combinedSample = append(combinedSample, b.head...)
	combinedSample = append(combinedSample, tailBytes...)

	if b.hasNUL || isMostlyBinaryUTF8(combinedSample) {
		return fmt.Sprintf("[binary output, %d bytes, omitted — write it to a file instead]", b.total), b.total, true
	}

	maxKept := int64(b.headCap + b.tailCap)
	if b.total <= maxKept {
		raw := make([]byte, 0, len(b.head)+len(tailBytes))
		raw = append(raw, b.head...)
		raw = append(raw, tailBytes...)
		return sanitizeTerminalText(string(trimValidUTF8End(raw))), b.total, false
	}

	omitted := b.total - int64(len(b.head)) - int64(len(tailBytes))
	headStr := sanitizeTerminalText(string(trimValidUTF8End(b.head)))
	tailStr := sanitizeTerminalText(string(trimValidUTF8Start(tailBytes)))
	return fmt.Sprintf("%s\n…[%d bytes omitted]…\n%s", headStr, omitted, tailStr), b.total, true
}

func (b *headTailBuffer) orderedTail() []byte {
	if b.tail == nil {
		return nil
	}
	if !b.tailFilled {
		return b.tail[:b.tailPos]
	}
	out := make([]byte, b.tailCap)
	n := copy(out, b.tail[b.tailPos:])
	copy(out[n:], b.tail[:b.tailPos])
	return out
}

func isMostlyBinaryUTF8(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	validBytes := 0
	for i := 0; i < len(sample); {
		r, size := utf8.DecodeRune(sample[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		// Control chars other than tab/newline/CR/ESC count against text ratio.
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != 0x1b {
			i += size
			continue
		}
		validBytes += size
		i += size
	}
	return float64(validBytes)/float64(len(sample)) < 0.70
}

func sanitizeTerminalText(s string) string {
	s = ansiOSCRegex.ReplaceAllString(s, "")
	s = ansiCSIRegex.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.ContainsRune(s, '\r') {
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			if idx := strings.LastIndexByte(line, '\r'); idx >= 0 {
				lines[i] = line[idx+1:]
			}
		}
		s = strings.Join(lines, "\n")
	}
	return s
}

// trimValidUTF8End removes ONLY a genuinely incomplete multi-byte UTF-8 sequence split
// at the very end of b (e.g. from a fixed-byte cut), without dropping valid trailing ASCII
// when an unrelated invalid byte exists earlier in the buffer.
func trimValidUTF8End(b []byte) []byte {
	n := len(b)
	if n == 0 {
		return b
	}
	// Walk back at most utf8.UTFMax-1 bytes to find the start of the final rune.
	start := n - 1
	limit := max(0, n-utf8.UTFMax)
	for start > limit && !utf8.RuneStart(b[start]) {
		start--
	}
	tail := b[start:]
	// Single-byte ASCII or complete multi-byte rune: keep everything.
	if tail[0] < utf8.RuneSelf || utf8.FullRune(tail) {
		return b
	}
	// If tail starts with a valid UTF-8 leading byte (0xC2..0xF4) whose continuation
	// bytes were cut off at the buffer boundary, drop just that incomplete fragment.
	if tail[0] >= 0xC2 && tail[0] <= 0xF4 {
		// Verify all bytes after tail[0] are continuation bytes (0x80..0xBF).
		allCont := true
		for _, c := range tail[1:] {
			if (c & 0xC0) != 0x80 {
				allCont = false
				break
			}
		}
		if allCont {
			return b[:start]
		}
	}
	return b
}

func trimValidUTF8Start(b []byte) []byte {
	start := 0
	for start < len(b) && start < utf8.UTFMax && !utf8.RuneStart(b[start]) {
		start++
	}
	return trimValidUTF8End(b[start:])
}
