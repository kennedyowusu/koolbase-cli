package cmd

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"os"
)

func generatePatch(info *snapshotInfo, privateKeyPath string, slotIndex int64) ([]byte, error) {
	privKeyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}
	if len(privKeyBytes) != 64 {
		return nil, fmt.Errorf("invalid private key: expected 64 bytes, got %d", len(privKeyBytes))
	}

	privKey := ed25519.PrivateKey(privKeyBytes)

	// Build 64-byte header
	header := make([]byte, 64)
	copy(header[0:4], []byte("KBPM"))
	binary.LittleEndian.PutUint16(header[4:6], 1)
	binary.LittleEndian.PutUint16(header[6:8], 64)
	binary.LittleEndian.PutUint32(header[8:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], 0)
	binary.LittleEndian.PutUint64(header[16:24], info.buildID)
	binary.LittleEndian.PutUint64(header[24:32], uint64(slotIndex))
	binary.LittleEndian.PutUint64(header[32:40], info.nmSnap)
	binary.LittleEndian.PutUint64(header[40:48], info.nmReplace)
	binary.LittleEndian.PutUint32(header[48:52], 1)
	binary.LittleEndian.PutUint32(header[52:56], 0)
	binary.LittleEndian.PutUint64(header[56:64], info.instrSize)

	sig := ed25519.Sign(privKey, header)

	patch := append(header, sig...)
	return patch, nil
}
