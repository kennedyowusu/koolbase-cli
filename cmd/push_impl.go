package cmd

import (
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
)

type snapshotInfo struct {
	buildID   uint64
	nmSnap    uint64
	nmTarget  uint64
	nmReplace uint64
	instrSize uint64
}

func analyzeSnapshot(snapshotPath, targetSym, replaceSym string) (*snapshotInfo, error) {
	f, err := macho.Open(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open snapshot: %w", err)
	}
	defer f.Close()

	syms := f.Symtab
	if syms == nil {
		return nil, fmt.Errorf("no symbol table found in snapshot")
	}

	var nmSnap, nmTarget, nmReplace uint64
	targetFound, replaceFound := false, false

	for _, s := range syms.Syms {
		name := s.Name
		if len(name) > 0 && name[0] == '_' {
			name = name[1:]
		}
		switch name {
		case "_kDartIsolateSnapshotInstructions", "kDartIsolateSnapshotInstructions":
			nmSnap = s.Value
		}
		if name == targetSym {
			nmTarget = s.Value
			targetFound = true
		}
		if name == replaceSym {
			nmReplace = s.Value
			replaceFound = true
		}
	}

	if !targetFound {
		return nil, fmt.Errorf("target symbol not found: %s", targetSym)
	}
	if !replaceFound {
		return nil, fmt.Errorf("replace symbol not found: %s", replaceSym)
	}

	instrSize, fileOffset, err := findInstructionsSection(f, nmSnap)
	if err != nil {
		return nil, err
	}

	buildID, err := computeBuildID(snapshotPath, fileOffset, instrSize)
	if err != nil {
		return nil, err
	}

	return &snapshotInfo{
		buildID:   buildID,
		nmSnap:    nmSnap,
		nmTarget:  nmTarget,
		nmReplace: nmReplace,
		instrSize: instrSize,
	}, nil
}

func findInstructionsSection(f *macho.File, nmSnap uint64) (size, fileOffset uint64, err error) {
	for _, l := range f.Loads {
		seg, ok := l.(*macho.Segment)
		if !ok {
			continue
		}
		if nmSnap >= seg.Addr && nmSnap < seg.Addr+seg.Filesz {
			size = seg.Addr + seg.Filesz - nmSnap
			fileOffset = seg.Offset + (nmSnap - seg.Addr)
			return
		}
	}
	err = fmt.Errorf("could not find instructions section for symbol 0x%x", nmSnap)
	return
}

func computeBuildID(path string, fileOffset, size uint64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, int64(fileOffset)); err != nil {
		return 0, fmt.Errorf("failed to read instructions: %w", err)
	}

	hash := sha256.Sum256(buf)
	return binary.LittleEndian.Uint64(hash[:8]), nil
}
