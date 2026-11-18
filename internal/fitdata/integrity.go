package fitdata

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/muktihari/fit/kit/hash/crc16"
)

// errIntegrity marks a FIT file whose size or CRC does not match its
// content: a truncated or damaged file.
var errIntegrity = errors.New("fitdata: integrity check failed")

// CheckIntegrity checks the first FIT file in b as the FIT SDK's
// CheckIntegrity does: a 14-byte header's CRC covers bytes 0–11 unless it is
// 0x0000 (the field is optional), and the file CRC covers the header and the
// data records. Decode runs it before decoding; an error means b is no whole,
// undamaged FIT file.
//
// The decoder's own check restarts the file CRC after a 14-byte header. That
// equals the SDK's only when the header CRC is set (a CRC run over a block
// and its own CRC ends at 0), so it rejects valid files with header CRC
// 0x0000, which Zwift writes for some rides.
func CheckIntegrity(b []byte) error {
	if len(b) < 12 {
		return fmt.Errorf("%w: %d bytes is shorter than a FIT header", errIntegrity, len(b))
	}
	size := int(b[0])
	if (size != 12 && size != 14) || len(b) < size {
		return fmt.Errorf("%w: header size %d", errIntegrity, size)
	}
	end := size + int(binary.LittleEndian.Uint32(b[4:8]))
	if end < size || len(b) < end+2 {
		return fmt.Errorf("%w: %d bytes, header says %d", errIntegrity, len(b), end+2)
	}
	if size == 14 {
		if h := binary.LittleEndian.Uint16(b[12:14]); h != 0 && h != crcOf(b[:12]) {
			return fmt.Errorf("%w: header CRC", errIntegrity)
		}
	}
	if binary.LittleEndian.Uint16(b[end:end+2]) != crcOf(b[:end]) {
		return fmt.Errorf("%w: file CRC", errIntegrity)
	}
	return nil
}

func crcOf(b []byte) uint16 {
	h := crc16.New()
	h.Write(b)
	return h.Sum16()
}
