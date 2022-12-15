package cryptorand

import (
	"crypto/rand"
	"encoding/binary"
	"k8s.io/klog/v2"
	"math/big"
)

func Intn(n int64) uint64 {
	val := new(big.Int).SetInt64(n)
	randNum, err := rand.Int(rand.Reader, val)
	if err != nil {
		klog.Errorf("Error generating random number using crypto/rand : %v", err)
		return 0
	}
	return randNum.Uint64()
}

func Uint32() uint32 {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		klog.Errorf("Error reading bytes for random number generation using crypto/rand: %v", err)
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func Uint64() uint64 {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		klog.Errorf("Error reading bytes for random number generation using crypto/rand: %v", err)
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func Read(randBytes []byte) []byte {
	_, err := rand.Read(randBytes)
	if err != nil {
		klog.Errorf("Error reading bytes using crypto/rand: %v", err)
		return nil
	}
	return randBytes
}
