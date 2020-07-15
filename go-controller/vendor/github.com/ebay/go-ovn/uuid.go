package goovn

import (
	"encoding/hex"

	"github.com/google/uuid"
)

func encodeHex(dst []byte, id uuid.UUID) {
	hex.Encode(dst, id[:4])
	dst[8] = '_'
	hex.Encode(dst[9:13], id[4:6])
	dst[13] = '_'
	hex.Encode(dst[14:18], id[6:8])
	dst[18] = '_'
	hex.Encode(dst[19:23], id[8:10])
	dst[23] = '_'
	hex.Encode(dst[24:], id[10:])
}

func newRowUUID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	var buf [36 + 3]byte
	copy(buf[:], "row")
	encodeHex(buf[3:], id)
	return string(buf[:]), nil
}
